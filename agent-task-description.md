# Autonomous Implementation Prompt: PostgresMemoryPolicy Kubernetes Operator (Go)

## Mission

You are an autonomous software engineering agent. Your goal is to implement a production-grade Kubernetes operator in Go from scratch. You must validate every step of your work, iterate on failures, and not stop until the operator is fully implemented, tested, and buildable.

---

## Operator Overview

Implement a Kubernetes operator called **`zalando-vertical-autoscaler`** that:

- Watches a custom resource `PostgresMemoryPolicy`
- Reads memory (and CPU) recommendations from VPA `.status.recommendation`
- Applies those recommendations to a **Zalando PostgreSQL operator** CR (`postgresql.acid.zalan.do`) during a defined maintenance window
- After successful PG cluster update, executes **post-actions** (e.g. `RolloutRestart`) against configured workloads
- Exposes full observability via CR status and Kubernetes events

---

## Technology Stack

- **Language**: Go 1.24
- **Framework**: [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime) (kubebuilder patterns)
- **CRD scaffolding**: Use `kubebuilder` CLI conventions but implement manually — do not rely on kubebuilder binary being present
- **Zalando CRD**: Use dynamic client or generate typed client from Zalando's CRD schema for `postgresql.acid.zalan.do/v1`
- **VPA types**: Import or vendor from `k8s.io/autoscaler/vertical-pod-autoscaler`
- **Build**: `Dockerfile` using `gcr.io/distroless/static` as final image
- **Manifests**: Helm chart under `charts/zalando-vertical-autoscaler`

---

## Custom Resource Definition

### `PostgresMemoryPolicy` (`postgresmemorypolicies.pricefx.io/v1alpha1`)

```go
type PostgresMemoryPolicySpec struct {
    // Reference to the Zalando postgresql CR (same namespace)
    TargetCluster string `json:"targetCluster"`

    // Reference to VPA object that holds recommendations
    VPAName string `json:"vpaName"`

    // Lower bound for memory requests
    MemoryMin resource.Quantity `json:"memoryMin"`

    // Upper bound for memory requests
    MemoryMax resource.Quantity `json:"memoryMax"`

    // Limits = Requests * Overcommit. Default: 1. Must be >= 1.
    // +kubebuilder:default=1
    // +kubebuilder:validation:Minimum=1
    Overcommit float64 `json:"overcommit,omitempty"`

    MaintenanceWindow MaintenanceWindowSpec `json:"maintenanceWindow"`

    // Safety gates evaluated before starting maintenance
    SafetyGates SafetyGatesSpec `json:"safetyGates,omitempty"`

    // Actions to execute after a successful PG cluster update, in order
    PostActions []PostActionSpec `json:"postActions,omitempty"`
}

type MaintenanceWindowSpec struct {
    // Standard cron expression (5-field)
    Cron string `json:"cron"`

    // Maximum duration of the maintenance window in minutes
    // +kubebuilder:default=60
    TimeoutMinutes int `json:"timeoutMinutes,omitempty"`
}

type SafetyGatesSpec struct {
    // If true, abort maintenance if Zalando cluster is not healthy
    // +kubebuilder:default=true
    RequireHealthyCluster bool `json:"requireHealthyCluster,omitempty"`

    // Minimum number of ready replicas required before proceeding
    // +kubebuilder:default=1
    MinReadyReplicas int32 `json:"minReadyReplicas,omitempty"`
}

// PostActionSpec defines a single action to execute after a successful maintenance run.
type PostActionSpec struct {
    // Action to perform on the target resource.
    // +kubebuilder:validation:Enum=RolloutRestart
    Action PostActionType `json:"action"`

    // Target resource to act upon.
    Target ActionTargetRef `json:"target"`
}

// PostActionType is the action verb, modelled after kubectl imperative commands.
// +kubebuilder:validation:Enum=RolloutRestart
type PostActionType string

const (
    // PostActionRolloutRestart triggers a rolling restart of the target workload,
    // equivalent to `kubectl rollout restart`.
    PostActionRolloutRestart PostActionType = "RolloutRestart"
)

// ActionTargetRef identifies a workload resource to act upon.
type ActionTargetRef struct {
    // Kind of the target resource.
    // +kubebuilder:validation:Enum=Deployment;StatefulSet;DaemonSet
    Kind string `json:"kind"`

    // Name of the target resource.
    Name string `json:"name"`

    // Namespace of the target resource. Defaults to the same namespace as the
    // PostgresMemoryPolicy CR if omitted.
    // +optional
    Namespace string `json:"namespace,omitempty"`
}

type PostgresMemoryPolicyStatus struct {
    // Recommendation read from VPA, clamped to min/max
    MemoryTarget *resource.Quantity `json:"memoryTarget,omitempty"`

    // Currently applied memory request in Zalando CR
    CurrentMemory *resource.Quantity `json:"currentMemory,omitempty"`

    // History of last 10 maintenance runs
    MaintenanceHistory []MaintenanceRecord `json:"maintenanceHistory,omitempty"`

    // Standard condition types
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type MaintenanceRecord struct {
    StartedAt      metav1.Time        `json:"startedAt"`
    CompletedAt    *metav1.Time       `json:"completedAt,omitempty"`
    Status         MaintenanceStatus  `json:"status"` // Pending | InProgress | Completed | Failed | Skipped
    Reason         string             `json:"reason,omitempty"`
    PreviousMemory string             `json:"previousMemory,omitempty"`
    AppliedMemory  string             `json:"appliedMemory,omitempty"`
}

// Condition types
const (
    ConditionMaintenanceInProgress = "MaintenanceInProgress"
    ConditionLastMaintenanceFailed = "LastMaintenanceFailed"
    ConditionVPARecommendationReady = "VPARecommendationReady"
)
```

---

## Reconciliation Logic

### Reconcile Loop (High Level)

```
1. Fetch PostgresMemoryPolicy CR
2. Sync VPA recommendation → status.memoryTarget (clamped to min/max)
3. Evaluate whether current time is within maintenance window (cron)
4. If NOT in window → requeue until next window opens, update status
5. If IN window:
   a. Check if maintenance already InProgress → if yes, continue monitoring
   b. Check if previous maintenance is still running → if yes, skip with Skipped record
   c. Evaluate safety gates
      - If requireHealthyCluster=true → check Zalando cluster status
      - If unhealthy → record Skipped with reason, requeue
   d. Start maintenance:
      - Set ConditionMaintenanceInProgress=True
      - Write MaintenanceRecord{status: InProgress, startedAt: now}
      - **Evaluate change gates** before patching (both conditions must be true):
        - Absolute diff: `abs(memoryTarget - currentMemory) > 5Gi` — skip with Skipped record and reason `ChangeGateAbsoluteDiff` if not met
        - Relative diff: `abs(memoryTarget - currentMemory) / currentMemory > 10%` — skip with Skipped record and reason `ChangeGateRelativeDiff` if not met
        - If either gate blocks the update, set `ConditionMaintenanceInProgress=False` and requeue until next window
      - Patch Zalando postgresql CR resources (memory + cpu)
      - Watch Zalando cluster status until Ready or timeout
      - If timeout → record Failed, set ConditionLastMaintenanceFailed=True
      - If Ready → proceed to post-actions
   e. Post-actions (executed in order):
      - For each entry in PostActions:
        - Dispatch to the appropriate action handler based on `.action`
        - `RolloutRestart`: fetch the target workload (Deployment / StatefulSet / DaemonSet),
          patch it with the rollout restart annotation, wait for rollout to complete
        - Namespace defaults to the CR namespace if `.target.namespace` is empty
      - If any post-action fails → record Failed and stop processing remaining actions
   f. On success:
      - Record MaintenanceRecord{status: Completed}
      - Set ConditionMaintenanceInProgress=False
      - Update status.currentMemory
      - Trim maintenanceHistory to last 10 entries
6. Requeue after computed duration until next cron window
```

### Zalando CR Patch Logic

When patching the Zalando `postgresql` CR:

- Set `spec.resources.requests.memory` = `memoryTarget`
- Set `spec.resources.limits.memory` = `memoryTarget * overcommit`
- CPU: read CPU recommendation from VPA (same container), apply same overcommit logic
- Use **merge patch** (`application/merge-patch+json`)
- Do NOT overwrite any other fields in the Zalando CR

### VPA Recommendation Reading

#### Locating the VPA object

The operator finds the VPA object using **`spec.vpaName`** from the `PostgresMemoryPolicy` CR. The VPA is always looked up in the **same namespace** as the `PostgresMemoryPolicy` CR — no cross-namespace lookup is supported.

```go
vpa := &vpav1.VerticalPodAutoscaler{}
err := r.Get(ctx, types.NamespacedName{
    Namespace: policy.Namespace,   // same namespace as the CR
    Name:      policy.Spec.VPAName, // from spec.vpaName field
}, vpa)
```

If the VPA object does not exist (not found error), set `ConditionVPARecommendationReady=False` with reason `VPANotFound` and requeue after 5 minutes.

#### Reading the recommendation

- Read from `VPA.status.recommendation.containerRecommendations`
- Target the container named `postgres` (most common in Zalando) — make container name configurable via `spec.vpaContainerName`, defaulting to `"postgres"`
- Use `.target.memory` (not upperBound or lowerBound)
- Clamp: `max(memoryMin, min(memoryMax, target))`
- If VPA has no recommendation yet (`.status.recommendation` is nil or empty) → set `ConditionVPARecommendationReady=False` with reason `NoRecommendationYet`, requeue after 5 minutes

### Post-Actions Execution

Post-actions are executed sequentially after a successful PG cluster update. The action handler is selected by `PostActionSpec.Action`.

#### `RolloutRestart`

Supported target kinds: `Deployment`, `StatefulSet`, `DaemonSet`.

Apply the standard rollout restart annotation patch:
```go
patch := fmt.Sprintf(
    `{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":"%s"}}}}}`,
    time.Now().Format(time.RFC3339),
)
```

- Resolve namespace: use `target.namespace` if set, otherwise fall back to `policy.Namespace`
- Respect the workload's own `.spec.strategy` / `.spec.updateStrategy` — do not override it
- Wait for rollout completion:
  - **Deployment**: `updatedReplicas == replicas && availableReplicas == replicas`
  - **StatefulSet**: `updatedReplicas == replicas && readyReplicas == replicas`
  - **DaemonSet**: `updatedNumberScheduled == desiredNumberScheduled && numberReady == desiredNumberScheduled`
- Per-target timeout: 10 minutes (hardcoded, not configurable for now)
- If an unknown `kind` is encountered, fail the action immediately with a descriptive error

#### Adding future action types

To add a new action type (e.g. `ScaleDown`, `ExecJob`):
1. Add a new constant to `PostActionType`
2. Update the `+kubebuilder:validation:Enum` markers on both `PostActionType` and `PostActionSpec.Action`
3. Implement the handler in `internal/controller/postactions.go` and dispatch from the main handler switch

---

## Cron Window Evaluation

- Use [robfig/cron](https://github.com/robfig/cron) v3 for cron parsing
- A maintenance window is **open** if: `now >= prevFire && now < prevFire + timeoutMinutes`
  - `robfig/cron` v3 only provides `Next()`; compute `prevFire` by walking forward from `now - 366d`
- Requeue duration when outside window = `nextFire - now + 5s jitter`

---

## RBAC Requirements

The operator ServiceAccount needs:

```yaml
# Read/write own CRD
- postgresmemorypolicies: get, list, watch, update, patch (all verbs for status subresource too)
# Read VPA
- verticalpodautoscalers: get, list, watch
# Read/patch Zalando CR
- postgresqls.acid.zalan.do: get, list, watch, patch, update
# Read/patch/watch workloads
- deployments: get, list, watch, patch, update
- statefulsets: get, list, watch, patch, update
- daemonsets: get, list, watch, patch, update
# Emit events
- events: create, patch
```

---

## Project Structure

```
zalando-vertical-autoscaler/
├── cmd/
│   └── main.go                        # manager setup, scheme registration
├── api/
│   └── v1alpha1/
│       ├── types.go                   # CRD types
│       ├── zz_generated.deepcopy.go   # generated deepcopy
│       └── groupversion_info.go
├── internal/
│   └── controller/
│       ├── postgresmemorpolicy_controller.go   # main reconciler
│       ├── vpa.go                              # VPA recommendation reader
│       ├── zalando.go                          # Zalando CR patcher
│       ├── postactions.go                      # post-action dispatcher and handlers
│       ├── cron.go                             # maintenance window evaluator
│       └── conditions.go                       # condition helpers
├── config/
│   └── crd/
│       └── postgresmemorpolicies.yaml          # CRD manifest
├── charts/
│   └── zalando-vertical-autoscaler/
│       ├── Chart.yaml
│       ├── values.yaml
│       └── templates/
│           ├── deployment.yaml
│           ├── rbac.yaml
│           ├── crd.yaml
│           └── serviceaccount.yaml
├── Dockerfile
├── go.mod
└── go.sum
```

---

## Validation & Testing Requirements

For each implementation step, you must validate before proceeding:

### 1. Build Validation
```bash
go build ./...          # must pass with zero errors
go vet ./...            # must pass with zero warnings
```

### 2. Unit Tests (required for these components)
- `cron.go` — window open/closed logic, requeue duration calculation
- `vpa.go` — recommendation clamping (below min, above max, within bounds, missing recommendation)
- `zalando.go` — patch payload correctness, overcommit calculation, change gate evaluation (both thresholds independently and combined)
- `conditions.go` — condition set/update helpers

### 3. Controller Test (envtest)
Use `sigs.k8s.io/controller-runtime/pkg/envtest` to write at least these integration scenarios:

| Scenario | Expected Outcome |
|---|---|
| VPA has no recommendation | ConditionVPARecommendationReady=False, requeue |
| Outside maintenance window | No changes, requeue at correct time |
| Inside window, cluster unhealthy, requireHealthyCluster=true | Skipped MaintenanceRecord |
| Inside window, cluster healthy, change < 5Gi or < 10% | Skipped MaintenanceRecord (change gate) |
| Inside window, cluster healthy | InProgress → Completed, status updated |
| Maintenance timeout | Failed MaintenanceRecord |
| Post-actions succeed (RolloutRestart) | Completed record, all targets restarted |
| Overlapping maintenance run | Second run skipped |

### 4. Manifest Validation
```bash
kubectl apply --dry-run=server -f config/crd/postgresmemorpolicies.yaml
helm lint charts/zalando-vertical-autoscaler
helm template charts/zalando-vertical-autoscaler | kubectl apply --dry-run=client -f -
```

---

## Engineering Standards

You are expected to write code as a **professional, senior Go engineer** would. This is not negotiable. Specifically:

### Code Quality
- Follow idiomatic Go: meaningful names, short functions with a single responsibility, avoid deep nesting
- Keep the reconciler thin — delegate to focused helpers (`vpa.go`, `zalando.go`, `cron.go`, etc.)
- No magic numbers or strings — define named constants for all thresholds, reason strings, annotation keys, etc.
- All exported types and functions must have godoc comments
- Errors must be wrapped with context: `fmt.Errorf("reading VPA recommendation: %w", err)`
- Avoid global state; pass dependencies explicitly via struct fields

### Unit Tests as a Verification Gate
Every non-trivial piece of logic **must have unit tests written immediately after the implementation** — do not defer tests to a later step. Treat a function without tests as unfinished. Each unit test must:
- Cover the happy path and all meaningful edge cases
- Use table-driven tests (`[]struct{ name, input, expected }`) where there are multiple scenarios
- Be runnable in isolation with `go test ./internal/controller/... -run TestXxx`
- Not depend on a running cluster or external services (mock/stub at the interface boundary)

### Test-Before-Proceed Rule
After implementing each component, run its unit tests and fix all failures before moving to the next component. The sequence is strictly:

```
implement → test locally → fix failures → confirm green → proceed
```

Never accumulate untested code across multiple components.

---

## Iteration Instructions for the Agent

1. **Start with types and CRD** — define all Go types, generate deepcopy, produce CRD YAML. Validate with `go build` and `kubectl apply --dry-run`.
2. **Implement cron window logic** — write and unit test in isolation before touching the reconciler.
3. **Implement VPA reader** — unit test all clamping edge cases.
4. **Implement Zalando patcher** — unit test patch payload, overcommit math, and change gate evaluation.
5. **Implement post-actions** — unit test `RolloutRestart` annotation patch, per-kind readiness wait logic, and unknown kind error handling.
6. **Wire reconciler** — compose all components into the reconcile loop.
7. **Write envtest integration tests** — cover all scenarios in the table above.
8. **Write Helm chart** — lint and template validate.
9. **Write Dockerfile** — build and verify image produces a runnable binary.
10. **Final check** — `go build ./...`, `go vet ./...`, `go test ./...` all green.

If any step fails, **debug and fix before proceeding to the next step**. Do not skip validation gates.

---

## Out of Scope (Do Not Implement)

- Alerting / PagerDuty / Slack notifications — handled externally
- Multi-cluster support
- Webhook defaulting/validation (can be added later)
- Metrics endpoint (can be added later)
- Reimplementing VPA recommendation algorithm — always read from VPA status

---

## Key Dependencies (go.mod)

```
sigs.k8s.io/controller-runtime v0.19.3
k8s.io/client-go v0.31.3
k8s.io/apimachinery v0.31.3
k8s.io/api v0.31.3
github.com/robfig/cron/v3 v3.0.1
k8s.io/autoscaler/vertical-pod-autoscaler v1.0.0
```

> If Zalando Go types are difficult to vendor, use `dynamic client` + `unstructured` to patch the `postgresql` CR — this is acceptable and avoids dependency issues.