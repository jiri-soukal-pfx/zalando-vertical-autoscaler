# Zalando Vertical Autoscaler

A Kubernetes operator that automatically applies [VPA](https://github.com/kubernetes/autoscaler/tree/master/vertical-pod-autoscaler) memory recommendations to [Zalando PostgreSQL](https://github.com/zalando/postgres-operator) clusters during scheduled maintenance windows.

## Why?

Zalando's Postgres operator does not natively integrate with the Kubernetes Vertical Pod Autoscaler. Changing memory on a Zalando `postgresql` CR triggers a rolling update of the StatefulSet, which means downtime risk. This operator bridges that gap by:

- Reading VPA memory (and optionally CPU) recommendations automatically
- Applying them only during **cron-based maintenance windows** you define
- Enforcing **safety gates** so small fluctuations don't trigger unnecessary restarts
- Running **post-actions** (e.g. rollout-restart of dependent workloads) after a successful update
- Tracking every maintenance run with full history and Kubernetes conditions

## How it works

```
                    VPA recommendation
                            │
                      Zalando CR has no memory?
                       no /          \ yes (and initialMemory set)
                      │          apply initialMemory immediately
                      │                    │
                      │               requeue (1m)
                      │
                 maintenance window open?
                  no /          \ yes
                 wait        safety gates pass?
                              no /       \ yes
                             skip    evaluate PG parameter templates
                                         │
                                   patch Zalando CR (memory + CPU + PG params)
                                         │
                                   wait for cluster Running
                                         │
                                   execute post-actions
                                         │
                                      done ✓
```

## Quick start

### Install with Helm

```bash
helm upgrade --install zalando-vpa \
  oci://ghcr.io/jiri-soukal-pfx/zalando-vertical-autoscaler/chart/zalando-vertical-autoscaler \
  --namespace operators --create-namespace
```

Or from a local clone:

```bash
helm upgrade --install zalando-vpa charts/zalando-vertical-autoscaler \
  --namespace operators --create-namespace
```

### Create a policy

```yaml
apiVersion: pricefx.io/v1alpha1
kind: PostgresMemoryPolicy
metadata:
  name: my-db-memory
  namespace: default
spec:
  # Zalando postgresql CR to manage
  targetCluster: my-zalando-pg

  # VPA object to read recommendations from (same namespace)
  vpaName: my-db-vpa

  # Clamp recommendations to this range
  memoryMin: 4Gi
  memoryMax: 64Gi

  # limits = requests * overcommit (default: 1)
  overcommit: 1

  # Add 20% headroom on top of VPA recommendation (default: 0)
  memoryBuffer: 20

  maintenanceWindow:
    # Every Sunday at 03:00 UTC
    cron: "0 3 * * 0"
    # Or: last Sunday of the month at 20:00 UTC
    # cron: "0 20 * * 0L"
    # Window stays open for 60 minutes
    timeoutMinutes: 60

  safetyGates:
    # Only proceed if the Zalando cluster reports "Running"
    requireHealthyCluster: true

  # Compute PG parameters from applied memory/CPU (optional)
  postgresParameters:
    shared_buffers: "{{ div (div .memory 3) 8192 }}"
    work_mem: "{{ div (div .memory 256) 1024 }}"
    max_parallel_workers_per_gather: "{{ div .cpu 2 }}"
    max_connections: "300"  # static values pass through as-is

  postActions:
    # Restart a dependent workload after the PG cluster is ready
    - action: RolloutRestart
      target:
        kind: Deployment
        name: my-app
```

## Safety gates

Before patching the Zalando CR, two change gates must **both** pass:

| Gate | Threshold | Purpose |
|------|-----------|---------|
| Absolute diff | > 5 GiB | Ignore small absolute changes |
| Relative diff | > 10% | Ignore small proportional changes |

If either gate blocks, the run is recorded as `Skipped` and the operator waits for the next window.

## Memory buffer

VPA recommendations can sometimes be too close to actual usage, leaving insufficient headroom for spikes. The `memoryBuffer` field adds a configurable percentage on top of the clamped VPA recommendation:

```
VPA recommends 20Gi → clamp to [4Gi, 64Gi] → 20Gi → apply 20% buffer → 24Gi applied
```

The buffered value is re-clamped to `memoryMax`, so the buffer can never push memory above the configured upper bound:

```
VPA recommends 60Gi → clamp to [4Gi, 64Gi] → 60Gi → apply 20% buffer → 72Gi → re-clamp → 64Gi applied
```

Set `memoryBuffer: 0` (the default) for no buffer. Valid range is 0–100.

## Observability

Everything is visible through standard Kubernetes mechanisms:

```bash
# Policy status with current/target memory and maintenance history
kubectl describe postgresmemorypolicy my-db-memory

# Kubernetes events
kubectl get events --field-selector involvedObject.name=my-db-memory
```

### Conditions

| Condition | Meaning |
|-----------|---------|
| `VPARecommendationReady` | A valid VPA recommendation exists |
| `MaintenanceInProgress` | Maintenance is currently running |
| `LastMaintenanceFailed` | The most recent run failed |

### Maintenance history

The last 10 runs are recorded in `.status.maintenanceHistory` with status, timing, previous and applied memory values, and failure reasons.

## Configuration reference

| Field | Default | Description |
|-------|---------|-------------|
| `spec.targetCluster` | *required* | Name of the Zalando `postgresql` CR |
| `spec.vpaName` | *required* | Name of the VPA object (same namespace) |
| `spec.vpaContainerName` | `postgres` | Container to read recommendations from |
| `spec.memoryMin` | *required* | Lower bound for memory |
| `spec.memoryMax` | *required* | Upper bound for memory |
| `spec.overcommit` | `1` | Memory limit multiplier (`limits = requests * overcommit`) |
| `spec.memoryBuffer` | `0` | Percentage (0–100) added on top of VPA recommendation after clamping. Re-clamped to `memoryMax`. |
| `spec.initialMemory` | - | Memory to apply when the Zalando CR has no resources set (bootstrap) |
| `spec.maintenanceWindow.cron` | *required* | 5-field cron expression (UTC). Supports `L` (last), `#` (nth), `W` (weekday) |
| `spec.maintenanceWindow.timeoutMinutes` | `60` | How long the window stays open |
| `spec.safetyGates.requireHealthyCluster` | `true` | Require cluster status `Running` |
| `spec.postActions[].action` | - | `RolloutRestart` |
| `spec.postActions[].target.kind` | - | `Deployment`, `StatefulSet`, or `DaemonSet` |
| `spec.postActions[].target.name` | - | Workload name |
| `spec.postActions[].target.namespace` | policy namespace | Override target namespace |
| `spec.postgresParameters` | - | Map of PG parameter names to Go template expressions (see below) |

## Cron syntax

Maintenance windows use [gronx](https://github.com/adhocore/gronx) for cron parsing, which supports standard 5-field expressions plus extended modifiers:

| Modifier | Field | Meaning | Example | Fires at |
|----------|-------|---------|---------|----------|
| `L` | day-of-week | Last occurrence in month | `0 20 * * 0L` | Last Sunday at 20:00 |
| `L` | day-of-month | Last day of month | `0 20 L * *` | Last day of month at 20:00 |
| `#` | day-of-week | Nth occurrence in month | `0 20 * * 0#2` | Second Sunday at 20:00 |
| `W` | day-of-month | Nearest weekday | `0 20 15W * *` | Weekday nearest 15th at 20:00 |

Standard expressions work as expected:

| Expression | Fires at |
|------------|----------|
| `0 3 * * 0` | Every Sunday at 03:00 |
| `0 2 * * 1-5` | Weekdays at 02:00 |
| `30 4 1 * *` | 1st of every month at 04:30 |

All times are evaluated in **UTC**. See the full [gronx documentation](https://github.com/adhocore/gronx#cron-expression) for details.

## PostgreSQL parameter templates

The operator can automatically compute PostgreSQL parameters based on the applied memory and CPU values. Define `spec.postgresParameters` as a map of parameter names to Go template expressions:

```yaml
spec:
  postgresParameters:
    # Templates receive .memory (bytes) and .cpu (whole cores)
    shared_buffers: "{{ div (div .memory 3) 8192 }}"
    work_mem: "{{ div (div .memory 256) 1024 }}"
    effective_cache_size: "{{ div (div (mul .memory 3) 4) 1024 }}kB"
    max_worker_processes: "{{ max 24 (add (div .cpu 2) .cpu) }}"
    max_parallel_workers_per_gather: "{{ div .cpu 2 }}"
    # Static values pass through unchanged
    max_connections: "300"
```

Evaluated parameters are patched into `spec.postgresql.parameters` on the Zalando CR alongside memory and CPU resources.

### Template inputs

| Variable | Type | Description |
|----------|------|-------------|
| `.memory` | int64 | Applied memory in bytes |
| `.cpu` | int64 | Applied CPU in whole cores (ceiling-rounded, e.g. 1600m -> 2) |

### Template functions

| Function | Signature | Description |
|----------|-----------|-------------|
| `div` | `div a b` | Integer division (`a / b`). Returns an error if `b` is zero. |
| `mul` | `mul a b` | Integer multiplication (`a * b`) |
| `add` | `add a b` | Integer addition (`a + b`) |
| `max` | `max a b` | Returns the larger of `a` and `b` |

### Error handling

- A typo in a variable name (e.g. `{{ .memroy }}`) produces a clear template error instead of silently rendering an empty value.
- Division by zero in `div` returns a reconciliation error instead of crashing the controller.
- Template errors fail the reconciliation gracefully and are reported via Kubernetes events.

## Design decisions

- **No CPU limits** -- CPU limits cause throttling. The operator sets CPU requests from VPA but never sets CPU limits.
- **Unstructured Zalando CR access** -- Uses dynamic client with merge patches to avoid importing Zalando operator Go types.
- **Cron-based windows** -- Changes are only applied during defined windows, never outside them. If the window expires mid-maintenance, the run is marked as failed.

## Development

```bash
# Run all tests (unit + integration)
./run-tests.sh

# Unit tests only (no envtest setup needed)
go test ./internal/controller/... -run 'Test[^C]' -v

# Build
go build ./...
```

## License

See [LICENSE](LICENSE) for details.
