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
VPA recommendation ──> clamp to [memoryMin, memoryMax]
                            │
                      maintenance window open?
                       no /          \ yes
                      wait        safety gates pass?
                                   no /       \ yes
                                  skip    patch Zalando CR
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

  maintenanceWindow:
    # Every Sunday at 03:00 UTC
    cron: "0 3 * * 0"
    # Window stays open for 60 minutes
    timeoutMinutes: 60

  safetyGates:
    # Only proceed if the Zalando cluster reports "Running"
    requireHealthyCluster: true

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
| `spec.maintenanceWindow.cron` | *required* | 5-field cron expression (UTC) |
| `spec.maintenanceWindow.timeoutMinutes` | `60` | How long the window stays open |
| `spec.safetyGates.requireHealthyCluster` | `true` | Require cluster status `Running` |
| `spec.postActions[].action` | - | `RolloutRestart` |
| `spec.postActions[].target.kind` | - | `Deployment`, `StatefulSet`, or `DaemonSet` |
| `spec.postActions[].target.name` | - | Workload name |
| `spec.postActions[].target.namespace` | policy namespace | Override target namespace |

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
