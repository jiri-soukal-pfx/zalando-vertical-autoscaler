# zalando-vertical-autoscaler

A production-grade Kubernetes operator that reads memory recommendations from
VPA and applies them to Zalando PostgreSQL operator CRs during a configured
maintenance window.

---

## What it does

- Watches `PostgresMemoryPolicy` CRs (`postgresmemorypolicies.pricefx.io/v1alpha1`)
- Reads `.status.recommendation` from a VPA object (same namespace)
- Clamps the recommendation to `[memoryMin, memoryMax]`
- Evaluates a cron-based maintenance window before making any change
- Applies memory (and optionally CPU) to a Zalando `postgresql.acid.zalan.do` CR
- Waits for the Zalando cluster to become `Running`
- Executes post-actions (currently only `RolloutRestart`) on configured workloads
- Exposes full observability via CR status conditions and Kubernetes events

---

## Repository layout

```
.
├── api/v1alpha1/           CRD types, deepcopy, groupversion_info
├── cmd/main.go             Manager entry point
├── config/
│   ├── crd/                CRD YAML manifest (install this in the cluster)
│   └── testdata/           Minimal VPA + Zalando CRDs for envtest only
├── internal/controller/    Reconciler and all helpers
│   ├── postgresmemorpolicy_controller.go   Main reconcile loop
│   ├── vpa.go / vpa_test.go                VPA reader + clamping
│   ├── zalando.go / zalando_test.go        Zalando CR patcher + change gates
│   ├── parameters.go / parameters_test.go   PG parameter template engine
│   ├── postactions.go / postactions_test.go RolloutRestart handler
│   ├── cron.go / cron_test.go              Maintenance window evaluator
│   ├── conditions.go / conditions_test.go  Condition/history helpers
│   ├── suite_test.go                       envtest Ginkgo suite bootstrap
│   └── integration_test.go                 envtest integration scenarios
├── charts/zalando-vertical-autoscaler/     Helm chart
├── .github/workflows/
│   ├── ci.yml              Build + test (required for PRs)
│   └── claude-code-review.yml  Claude AI code review on PRs
├── Dockerfile
└── agent-task-description.md  Original implementation spec (authoritative reference)
```

---

## Tech stack

| Concern | Choice |
|---|---|
| Language | Go 1.24 |
| Framework | controller-runtime v0.19.3 (k8s 1.31) |
| Cron parsing | robfig/cron v3 |
| VPA types | k8s.io/autoscaler/vertical-pod-autoscaler v1.0.0 |
| Zalando CR | Dynamic client (`unstructured`) — no typed Zalando dependency |
| Tests | stdlib `testing` (unit) + Ginkgo v2 / Gomega (integration/envtest) |
| Image | `gcr.io/distroless/static:nonroot` |
| Helm chart | `charts/zalando-vertical-autoscaler/` |

Go module: `github.com/pricefx/zalando-vertical-autoscaler`

---

## Key design decisions

### No CPU limits — ever
Do **not** set CPU limits anywhere (Kubernetes manifests, Helm values, operator
output). CPU limits cause throttling and are considered harmful in this project.
CPU *requests* are fine. This applies to:
- `charts/zalando-vertical-autoscaler/values.yaml` — only `limits.memory` is set
- The Zalando CR patch in `zalando.go` — only `requests.cpu` is set from the
  VPA CPU recommendation; `limits.cpu` is never written

### Zalando CR is patched with unstructured client
The Zalando `postgresql` CR is accessed via `unstructured.Unstructured` + merge
patch. This avoids importing the Zalando operator's Go types. The status field
`status.PostgresClusterStatus == "Running"` is the health/ready signal.

### Change gates (both must pass to proceed)
Before patching the Zalando CR the reconciler checks:
1. **Absolute diff** > 5 GiB
2. **Relative diff** > 10%

If either gate blocks, a `Skipped` maintenance record is written and the
reconciler requeues until the next window.

### Maintenance window model
A window is **open** if `now >= prevFire && now < prevFire + timeoutMinutes`.
`robfig/cron` v3 provides only `Next()`; `previousFire()` in `cron.go` walks
forward from `now - 366d` to find the most recent fire.

---

## Development workflow

### Run all tests (recommended)
```bash
./run-tests.sh
```
This script installs `setup-envtest`, downloads k8s 1.31 binaries, runs
`go mod tidy`, and executes **all** tests (unit + integration/envtest) in one
step. No prerequisites beyond Go 1.24+.

### Build & vet
```bash
go build ./...
go vet ./...
```

### Unit tests only (no cluster needed)
```bash
go test ./internal/controller/... -run 'Test[^C]' -v
```

### Integration tests only (envtest — requires k8s binaries)
```bash
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
export KUBEBUILDER_ASSETS=$(setup-envtest use 1.31 -p path)
go test ./internal/controller/... -run TestControllers -v -timeout 120s
```

### Helm lint
```bash
helm lint charts/zalando-vertical-autoscaler
helm template charts/zalando-vertical-autoscaler | kubectl apply --dry-run=client -f -
```

---

## Branch protection (main)

| Rule | Value |
|---|---|
| Required status check | `Build & Test` (ci.yml job) — must pass |
| Required PR reviews | 1 approval; stale reviews dismissed on new push |
| Direct push to main | Blocked for everyone including admins |
| Force push | Blocked |

All changes must go through a PR that passes CI and is approved.

---

## Helm chart notes

- Image repo: `ghcr.io/jiri-soukal-pfx/zalando-vertical-autoscaler`
- CRD install controlled by `installCRD: true` (default)
- Leader election disabled by default (`leaderElect: false`); enable for HA
  deployments with `replicaCount > 1`
- Resource defaults: `limits.memory=128Mi`, `requests.cpu=10m`,
  `requests.memory=64Mi` — no CPU limit by design

---

## Spec reference (agent-task-description.md)

The file `agent-task-description.md` is the authoritative spec. Key details
worth keeping in mind when extending the operator:

### RBAC the operator needs
```yaml
- postgresmemorypolicies: get, list, watch, update, patch (+ status subresource)
- verticalpodautoscalers: get, list, watch
- postgresqls.acid.zalan.do: get, list, watch, patch, update
- deployments, statefulsets, daemonsets: get, list, watch, patch, update
- events: create, patch
```

### VPA recommendation reading rules
- Container name defaults to `"postgres"`; configurable via `spec.vpaContainerName`
- Read `.target.memory` (not `upperBound` or `lowerBound`)
- VPA looked up in **same namespace** as the `PostgresMemoryPolicy` CR
- No recommendation → `ConditionVPARecommendationReady=False`, requeue after 5 min

### Zalando CR patch
- Set `spec.resources.requests.memory = memoryTarget`
- Set `spec.resources.limits.memory = memoryTarget × overcommit`
- CPU: request = VPA CPU target, **no CPU limit** (see "No CPU limits" above)
- Use merge patch — do NOT overwrite unrelated fields
- If `spec.initialMemory` is set and the Zalando CR has no `spec.resources.requests.memory`,
  the operator applies `initialMemory` immediately (no window check, no change gates).
  CPU is derived at 10:1 ratio (1 GiB → 100m CPU). This is a one-time bootstrap.

### PostgreSQL parameter templates (`spec.postgresParameters`)
When defined, the operator evaluates Go template expressions and patches the
results into `spec.postgresql.parameters` on the Zalando CR alongside memory/CPU.
- Template inputs: `.memory` (bytes, int64) and `.cpu` (whole cores, int64)
- Available functions: `div`, `mul`, `add`, `max` (all int64)
- `div` returns an error on division by zero (instead of panicking)
- Templates use `Option("missingkey=error")` — a typo like `{{ .memroy }}`
  fails with a clear error rather than silently rendering `<no value>`
- Values not starting with `{{` pass through as static strings
- Implementation: `internal/controller/parameters.go`

### RolloutRestart readiness criteria
- **Deployment**: `updatedReplicas == replicas && availableReplicas == replicas`
- **StatefulSet**: `updatedReplicas == replicas && readyReplicas == replicas`
- **DaemonSet**: `updatedNumberScheduled == desiredNumberScheduled && numberReady == desiredNumberScheduled`
- Per-target timeout: 10 minutes (hardcoded)

### Adding a new PostAction type
1. Add constant to `PostActionType` in `api/v1alpha1/types.go`
2. Update `+kubebuilder:validation:Enum` markers on both `PostActionType` and `PostActionSpec.Action`
3. Implement handler in `internal/controller/postactions.go`, add case to the dispatch switch

### Out of scope (do not implement)
- Alerting / PagerDuty / Slack
- Multi-cluster support
- Webhook defaulting/validation
- Metrics endpoint
- Reimplementing VPA recommendation algorithm

---

## Useful commands

```bash
# Apply CRD to a cluster
kubectl apply -f config/crd/postgresmemorpolicies.yaml

# Install via Helm
helm upgrade --install zalando-vpa charts/zalando-vertical-autoscaler \
  --namespace operators --create-namespace

# Watch operator logs
kubectl logs -n operators deploy/zalando-vertical-autoscaler -f

# Inspect policy status
kubectl get postgresmemorypolicies -A
kubectl describe postgresmemorypolicy <name> -n <ns>
```
