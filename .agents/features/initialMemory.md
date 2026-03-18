## Prompt: Implement `initialMemory` field in zalando-vertical-autoscaler

### Goal

Add an `initialMemory` field to `PostgresMemoryPolicySpec` that seeds the Zalando `postgresql` CR with initial resource values when it has no `spec.resources.requests.memory` set, with CPU derived automatically from this memory value. This bootstraps new PG clusters created without explicit resources (e.g., when a Helm chart deliberately omits `spec.resources` to delegate memory management to this operator).

### Context

When this operator manages a Zalando PostgreSQL CR's memory, the deploying Helm chart should omit `spec.resources` from the `acid.zalan.do/v1 postgresql` CR to avoid ArgoCD drift. But the PG cluster needs resources to start. The `initialMemory` field solves this: on the first reconcile, if the Zalando CR has no memory set, the operator applies `initialMemory` immediately — bypassing the maintenance window and change gates — as a one-time bootstrap.

### Repository structure (key files to modify)

```
api/v1alpha1/types.go                              # CRD types
config/crd/postgresmemorpolicies.yaml               # CRD YAML manifest
charts/zalando-vertical-autoscaler/files/postgresmemorpolicies.yaml  # CRD YAML (identical copy for Helm)
internal/controller/postgresmemorpolicy_controller.go  # Main reconcile loop
internal/controller/zalando.go                      # ZalandoPatcher (GetCurrentMemory, PatchResources)
internal/controller/zalando_test.go                 # Unit tests for zalando.go
internal/controller/integration_test.go             # envtest integration tests
CLAUDE.md                                           # Project documentation
README.md                                           # User-facing documentation
```

### Detailed implementation steps

#### 1. Add `InitialMemory` field to `PostgresMemoryPolicySpec` in `api/v1alpha1/types.go`

Add a new optional field to `PostgresMemoryPolicySpec`:

```go
// InitialMemory is the memory value applied to the Zalando CR when it has no
// spec.resources.requests.memory set. This bootstraps new clusters before VPA
// has produced a recommendation. Applied immediately, bypassing the maintenance
// window and change gates. CPU is derived automatically at a 10:1 memory-to-CPU
// ratio (e.g., 10Gi memory → 1000m CPU).
// +optional
InitialMemory *resource.Quantity `json:"initialMemory,omitempty"`
```

Place it after `Overcommit` and before `MaintenanceWindow`. The field is optional — if not set, the operator does not do initial seeding (existing behavior).

Run `go generate ./...` is NOT needed for this project — deepcopy is already generated via `zz_generated.deepcopy.go` and the `resource.Quantity` pointer type is handled by the existing deepcopy generator. However, verify that `zz_generated.deepcopy.go` correctly handles `*resource.Quantity` (it should, since `MemoryTarget` in the Status struct already uses this type).

#### 2. Update the CRD YAML manifests

Both `config/crd/postgresmemorpolicies.yaml` AND `charts/zalando-vertical-autoscaler/files/postgresmemorpolicies.yaml` (these must stay identical) need a new property under `spec.properties`:

```yaml
initialMemory:
  anyOf:
  - type: integer
  - type: string
  description: "InitialMemory is the memory value applied to the Zalando CR when it has no spec.resources.requests.memory set. This bootstraps new clusters before VPA has produced a recommendation. Applied immediately, bypassing the maintenance window and change gates."
  pattern: "^(\\+|-)?(([0-9]+(\\.[0-9]*)?)|(\\.['0-9]+))(([KMGTPE]i)|[numkMGTPE]|([eE](\\+|-)?(([0-9]+(\\.[0-9]*)?)|(\\.['0-9]+))))?$"
  x-kubernetes-int-or-string: true
```

This follows the exact same pattern as `memoryMin` and `memoryMax` in the existing CRD. Do NOT add `initialMemory` to the `required` list — it must be optional.

#### 3. Implement the bootstrap logic in `internal/controller/postgresmemorpolicy_controller.go`

In the `reconcilePolicy` method, add a new step **between Step 1 (VPA recommendation sync) and Step 2 (maintenance window evaluation)**. This is the key logic:

```go
// Step 1.5: Bootstrap — if the Zalando CR has no memory set and InitialMemory is configured,
// apply initial resources immediately (no window check, no change gates).
currentMemory, err := r.zalandoPatcher.GetCurrentMemory(ctx, policy.Namespace, policy.Spec.TargetCluster)
if err != nil {
    return ctrl.Result{}, fmt.Errorf("reading current memory for bootstrap check: %w", err)
}

if currentMemory == nil && policy.Spec.InitialMemory != nil {
    logger.Info("Zalando CR has no memory set, applying initialMemory", "initialMemory", policy.Spec.InitialMemory.String())
    
    // Build a synthetic VPARecommendation from InitialMemory.
    // CPU is derived at 10:1 ratio: memory in GiB * 100m.
    initialMemory := policy.Spec.InitialMemory.DeepCopy()
    memBytes := initialMemory.Value()
    cpuMillis := int64(float64(memBytes) / (1024 * 1024 * 1024) * 100)
    if cpuMillis < 100 {
        cpuMillis = 100 // minimum 100m
    }
    cpuQuantity := resource.NewMilliQuantity(cpuMillis, resource.DecimalSI)
    
    initialRec := &VPARecommendation{
        Memory: initialMemory,
        CPU:    cpuQuantity,
    }
    
    if err := r.zalandoPatcher.PatchResources(ctx, policy, initialRec); err != nil {
        return ctrl.Result{}, fmt.Errorf("applying initial memory: %w", err)
    }
    
    policy.Status.CurrentMemory = &initialMemory
    r.Recorder.Eventf(policy, "Normal", "InitialMemoryApplied",
        "applied initial memory=%s cpu=%s to cluster %q (no prior resources set)",
        initialMemory.String(), cpuQuantity.String(), policy.Spec.TargetCluster)
    
    // Requeue to continue normal reconciliation (VPA check, window check, etc.)
    return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
}
```

**Important design decisions for this step:**
- This runs BEFORE the maintenance window check — it's bootstrap, not maintenance
- This does NOT create a maintenance history record — it's not a maintenance run
- This does NOT check change gates — there's nothing to compare against
- The `Overcommit` factor from the policy spec IS applied (via `PatchResources` which already reads `policy.Spec.Overcommit`)
- CPU is derived from memory at 10:1 ratio (matching the Helm chart's `calculate_cpu_from_memory` helper: 1 GiB → 100m CPU), with a floor of 100m
- After applying, the reconciler requeues after 1 minute to continue the normal flow

**Note about `currentMemory` reuse:** The current code calls `GetCurrentMemory` later in Step 5 (change gates). After adding the bootstrap step, `currentMemory` is already fetched. If the bootstrap was NOT triggered (either `currentMemory != nil` or `InitialMemory == nil`), store the `currentMemory` value for reuse in Step 5 to avoid a second API call. Refactor Step 5 to skip its own `GetCurrentMemory` call by moving the declaration earlier and reusing it.

#### 4. Regenerate deepcopy

Run:
```bash
cd /home/soukal/GIT/GITHUB/zalando-vertical-autoscaler
go generate ./api/...
```

If the project doesn't use `go generate` for deepcopy, check how `zz_generated.deepcopy.go` is currently generated. The existing file already handles `*resource.Quantity` (see `Status.MemoryTarget`), so the pattern is established. If deepcopy generation isn't automated:
- Manually add to the `DeepCopyInto` method for `PostgresMemoryPolicySpec`:
```go
if in.InitialMemory != nil {
    in, out := &in.InitialMemory, &out.InitialMemory
    x := (*in).DeepCopy()
    *out = &x
}
```

#### 5. Add unit tests in `internal/controller/zalando_test.go`

No new unit tests needed in `zalando_test.go` since the bootstrap logic uses existing `PatchResources` and `GetCurrentMemory` which are already tested.

#### 6. Add integration tests in `internal/controller/integration_test.go`

Add these test scenarios:

**Scenario: InitialMemory applied when Zalando CR has no resources**
```
- Create VPA with recommendation 20Gi
- Create Zalando CR with NO spec.resources (empty spec)
- Create PostgresMemoryPolicy with initialMemory=8Gi, cronNeverOpen (window closed)
- Reconcile
- Assert: Zalando CR now has spec.resources.requests.memory = "8Gi"
- Assert: Zalando CR now has spec.resources.limits.memory = "8Gi" (overcommit=1)
- Assert: Zalando CR now has spec.resources.requests.cpu derived from 8Gi (800m)
- Assert: Event "InitialMemoryApplied" was recorded
- Assert: No maintenance history records (this is not maintenance)
- Assert: RequeueAfter = 1 minute
```

**Scenario: InitialMemory NOT applied when Zalando CR already has resources**
```
- Create VPA with recommendation 20Gi
- Create Zalando CR with spec.resources.requests.memory = "4Gi"
- Create PostgresMemoryPolicy with initialMemory=8Gi, cronNeverOpen
- Reconcile
- Assert: Zalando CR still has memory = "4Gi" (unchanged)
- Assert: Normal flow continues (requeues until window opens)
```

**Scenario: InitialMemory not set — existing behavior unchanged**
```
- Create VPA with recommendation 20Gi
- Create Zalando CR with NO spec.resources
- Create PostgresMemoryPolicy WITHOUT initialMemory, cronAlwaysOpen
- Reconcile
- Assert: Normal flow (change gates with nil currentMemory → proceeds to maintenance)
```

**Scenario: InitialMemory with overcommit > 1**
```
- Create Zalando CR with NO spec.resources
- Create PostgresMemoryPolicy with initialMemory=10Gi, overcommit=1.5
- Reconcile
- Assert: memory request = 10Gi, memory limit = 15Gi (10 * 1.5)
```

To create a Zalando CR without resources for tests, add a new helper or modify the existing `makeZalandoCluster`:

```go
func makeZalandoClusterNoResources(ctx context.Context, namespace, name, statusValue string) *unstructured.Unstructured {
    pg := &unstructured.Unstructured{}
    pg.SetGroupVersionKind(zalandoGVK)
    pg.SetName(name)
    pg.SetNamespace(namespace)
    // Minimal spec — no resources block
    Expect(unstructured.SetNestedField(pg.Object, map[string]interface{}{}, "spec")).To(Succeed())
    Expect(k8sClient.Create(ctx, pg)).To(Succeed())
    pg.Object["status"] = map[string]interface{}{
        "PostgresClusterStatus": statusValue,
    }
    Expect(k8sClient.Status().Update(ctx, pg)).To(Succeed())
    return pg
}
```

#### 7. Update documentation

**CLAUDE.md** — In the "Zalando CR patch" section, add:
```
- If `spec.initialMemory` is set and the Zalando CR has no `spec.resources.requests.memory`,
  the operator applies `initialMemory` immediately (no window check, no change gates).
  CPU is derived at 10:1 ratio (1 GiB → 100m CPU). This is a one-time bootstrap.
```

**README.md** — In the "Configuration reference" table, add:
```
| `spec.initialMemory` | - | Memory to apply when the Zalando CR has no resources set (bootstrap) |
```

In the "How it works" diagram, add a branch before the maintenance window check:
```
VPA recommendation ──> clamp to [memoryMin, memoryMax]
                            │
                      Zalando CR has no memory?
                       no /          \ yes (and initialMemory set)
                      │          apply initialMemory immediately
                      │                    │
                      │               requeue (1m)
                      │
                 maintenance window open?
                  ...
```

### What NOT to do

- Do NOT make `initialMemory` required — it must be optional
- Do NOT apply `initialMemory` during a maintenance window — it bypasses windows entirely  
- Do NOT create a maintenance history record for initial memory application
- Do NOT add CPU limits — the project explicitly prohibits CPU limits everywhere. Only set CPU requests.
- Do NOT add `initialCPU` as a separate field — derive CPU from `initialMemory` at 10:1 ratio
- Do NOT change the change gate thresholds (5Gi absolute, 10% relative)
- Do NOT modify the VPA reading logic — `initialMemory` is independent of VPA recommendations

### Verification

After implementation, run:
```bash
cd /home/soukal/GIT/GITHUB/zalando-vertical-autoscaler
./run-tests.sh
```

All existing tests must continue to pass. The new integration tests must also pass. Also run:
```bash
go vet ./...
helm lint charts/zalando-vertical-autoscaler
```
