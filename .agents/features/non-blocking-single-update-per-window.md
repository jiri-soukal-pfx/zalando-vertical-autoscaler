## Prompt: Refactor to non-blocking reconciler with single update per maintenance window

### Goal

Refactor the reconciler from a blocking model (where `startMaintenance` holds the reconcile goroutine for up to 1 hour) to a non-blocking state machine that advances through phases across multiple reconcile cycles. Additionally, enforce that only ONE Zalando CR patch happens per maintenance window — preventing multiple database restarts when VPA recommendations shift mid-window.

### Problem

In production, databases are restarted multiple times during a single maintenance window:

1. Window opens. VPA recommends 20Gi, current is 10Gi. Change gate passes. Operator patches Zalando CR.
2. `startMaintenance` blocks: patches CR, calls `WaitForClusterReady` (polling loop), runs post-actions (also blocking). DB restarts, eventually becomes Running.
3. Maintenance marked Completed. Reconciler requeues.
4. Still within the 1h window. VPA now recommends 25Gi (workload characteristics changed after restart). Current is 20Gi. 5Gi diff passes the absolute gate → operator patches again.
5. Another restart. This can repeat.

Secondary problem: the blocking design means an operator pod restart mid-maintenance loses all in-flight state (which phase we're in, whether we already patched, whether post-actions were triggered). The reconciler has no way to resume — it either re-patches or starts from scratch.

### Design

#### Non-blocking state machine

Add a `Phase` field to `MaintenanceRecord` to track progress within an `InProgress` maintenance:

| Phase | Meaning | Action on reconcile |
|---|---|---|
| `PatchApplied` | Zalando CR patched, waiting for cluster to become Running | Poll cluster health, requeue 30s |
| `PostActionsTriggered` | Cluster healthy, rollout restarts applied, waiting for rollout completion | Poll rollout status, requeue 30s |
| *(terminal)* | Record status becomes `Completed` or `Failed` | Done for this window |

#### One-update-per-window guard

Before starting a new maintenance, check if any `Completed` record in `MaintenanceHistory` has `StartedAt` within the current window bounds `[windowStart, windowEnd)`. If so, skip — requeue to the next window.

#### Window expiry with grace

When the reconciler fires and maintenance is `InProgress` but the window has expired:
- Check cluster health and post-action completion RIGHT NOW (single check, not a poll)
- If cluster is healthy AND post-actions are complete → mark `Completed` (grace period success)
- Otherwise → mark `Failed` (no revert — leave the DB in its current state)

#### No revert

When maintenance fails (timeout or error), do NOT revert to previous memory. A revert would trigger yet another restart, doubling disruption. The operator records the failure and tries again in the next maintenance window. The `PreviousMemory` field on the record is kept for observability, not for rollback.

### Repository structure (key files to modify)

```
api/v1alpha1/types.go                                # Add MaintenancePhase type + Phase field
config/crd/postgresmemorpolicies.yaml                 # Update CRD with phase field
charts/zalando-vertical-autoscaler/files/postgresmemorpolicies.yaml  # Same CRD update
internal/controller/postgresmemorpolicy_controller.go  # Main refactor: non-blocking state machine
internal/controller/postactions.go                     # Split into trigger + check
internal/controller/conditions.go                      # Add hasCompletedMaintenanceInWindow helper
internal/controller/zalando.go                         # WaitForClusterReady no longer called from reconcile
internal/controller/integration_test.go                # Update/add integration tests
internal/controller/conditions_test.go                 # Tests for new helpers
internal/controller/postactions_test.go                # Tests for split trigger/check
CLAUDE.md                                              # Update documentation
```

### Detailed implementation steps

#### 1. Add `MaintenancePhase` to `api/v1alpha1/types.go`

Add a new type and constants:

```go
// MaintenancePhase tracks progress within an InProgress maintenance run.
// +kubebuilder:validation:Enum=PatchApplied;PostActionsTriggered
type MaintenancePhase string

const (
    // MaintenancePhasePatchApplied means the Zalando CR has been patched and the
    // reconciler is waiting for the cluster to become Running.
    MaintenancePhasePatchApplied MaintenancePhase = "PatchApplied"

    // MaintenancePhasePostActionsTriggered means post-actions (e.g., rollout restarts)
    // have been triggered and the reconciler is waiting for them to complete.
    MaintenancePhasePostActionsTriggered MaintenancePhase = "PostActionsTriggered"
)
```

Add a `Phase` field to `MaintenanceRecord`:

```go
type MaintenanceRecord struct {
    // ... existing fields ...

    // Phase tracks the current step within an InProgress maintenance run.
    // Empty for terminal states (Completed, Failed, Skipped).
    // +optional
    Phase MaintenancePhase `json:"phase,omitempty"`
}
```

#### 2. Update CRD YAML manifests

Both `config/crd/postgresmemorpolicies.yaml` AND `charts/zalando-vertical-autoscaler/files/postgresmemorpolicies.yaml` need the `phase` property added under `maintenanceHistory.items.properties`:

```yaml
phase:
  description: "Phase tracks the current step within an InProgress maintenance run."
  type: string
  enum:
  - PatchApplied
  - PostActionsTriggered
```

These two files must remain identical.

#### 3. Add `hasCompletedMaintenanceInWindow` to `internal/controller/conditions.go`

```go
// hasCompletedMaintenanceInWindow returns true if any Completed maintenance record
// in the history has a StartedAt time that falls within the given window bounds.
// windowStart is inclusive, windowEnd is exclusive.
func hasCompletedMaintenanceInWindow(policy *policyv1alpha1.PostgresMemoryPolicy, windowStart, windowEnd time.Time) bool {
    for _, rec := range policy.Status.MaintenanceHistory {
        if rec.Status == policyv1alpha1.MaintenanceStatusCompleted &&
            !rec.StartedAt.Time.Before(windowStart) &&
            rec.StartedAt.Time.Before(windowEnd) {
            return true
        }
    }
    return false
}
```

When `WindowResult.InWindow == true`, `WindowResult.NextOpen` is the window start time, so the caller passes `windowResult.NextOpen` and `windowResult.WindowEnd`.

#### 4. Split post-actions in `internal/controller/postactions.go`

Split the current blocking methods into trigger and check:

**`TriggerPostActions`** — Apply restart annotations only, no waiting:

```go
// TriggerPostActions applies post-action triggers (e.g., restart annotations)
// without waiting for completion. Returns immediately.
func (e *PostActionExecutor) TriggerPostActions(ctx context.Context, policy *policyv1alpha1.PostgresMemoryPolicy) error {
    for _, action := range policy.Spec.PostActions {
        if err := e.trigger(ctx, policy, action); err != nil {
            return fmt.Errorf("triggering post-action %s on %s/%s: %w",
                action.Action, action.Target.Kind, action.Target.Name, err)
        }
    }
    return nil
}
```

The `trigger` method applies the restart annotation patch but does NOT call `waitXxxReady`.

**`ArePostActionsComplete`** — Check if all rollouts are done:

```go
// ArePostActionsComplete checks whether all post-action targets have finished
// their rollout. Returns true only if ALL targets are ready.
func (e *PostActionExecutor) ArePostActionsComplete(ctx context.Context, policy *policyv1alpha1.PostgresMemoryPolicy) (bool, error) {
    for _, action := range policy.Spec.PostActions {
        done, err := e.isComplete(ctx, policy, action)
        if err != nil {
            return false, err
        }
        if !done {
            return false, nil
        }
    }
    return true, nil
}
```

The `isComplete` method reads the target workload status and checks the readiness criteria (same as the existing `waitXxxReady` methods, but returning a bool instead of blocking):

- **Deployment**: `updatedReplicas == replicas && availableReplicas == replicas`
- **StatefulSet**: `updatedReplicas == replicas && readyReplicas == replicas`
- **DaemonSet**: `updatedNumberScheduled == desiredNumberScheduled && numberReady == desiredNumberScheduled`

If no post-actions are defined (`len(policy.Spec.PostActions) == 0`), return `true` immediately.

Keep the existing `Execute` method for backward compatibility with any callers, but the reconciler should use the new split methods.

#### 5. Refactor `internal/controller/postgresmemorpolicy_controller.go`

This is the main change. The `reconcilePolicy` method becomes:

```go
func (r *PostgresMemoryPolicyReconciler) reconcilePolicy(ctx context.Context, policy *policyv1alpha1.PostgresMemoryPolicy) (ctrl.Result, error) {
    logger := log.FromContext(ctx)

    // Step 1: Bootstrap (unchanged — initialMemory logic stays as-is)
    currentMemory, err := r.zalandoPatcher.GetCurrentMemory(ctx, policy.Namespace, policy.Spec.TargetCluster)
    if err != nil {
        return ctrl.Result{}, fmt.Errorf("reading current memory for bootstrap check: %w", err)
    }
    if currentMemory == nil && policy.Spec.InitialMemory != nil {
        // ... existing bootstrap logic, unchanged ...
    }

    // Step 2: Read VPA recommendation (unchanged)
    rec, err := r.vpaReader.ReadRecommendation(ctx, policy)
    // ... existing VPA logic, unchanged ...

    // Step 3: Evaluate maintenance window
    now := time.Now()
    windowResult, err := r.windowEvaluator.Evaluate(...)
    // ... existing window evaluation, unchanged ...

    // Step 4: If maintenance is in progress, advance the state machine
    if IsConditionTrue(policy, policyv1alpha1.ConditionMaintenanceInProgress) {
        inProgressRec := findInProgressRecord(policy)
        if inProgressRec != nil {
            if !windowResult.InWindow {
                // Window expired — check for grace period success
                return r.handleWindowExpired(ctx, policy, inProgressRec)
            }
            // Still in window — advance phases
            return r.monitorMaintenance(ctx, policy, inProgressRec, windowResult, now)
        }
        // Stale condition with no matching record — clear it
        SetCondition(policy, policyv1alpha1.ConditionMaintenanceInProgress,
            metav1.ConditionFalse, "StaleConditionCleared", "no in-progress record found")
    }

    // Step 5: One-update-per-window guard
    if windowResult.InWindow && hasCompletedMaintenanceInWindow(policy, windowResult.NextOpen, windowResult.WindowEnd) {
        logger.V(1).Info("maintenance already completed in this window, skipping")
        requeueAfter := RequeueAfter(windowResult, now)
        return ctrl.Result{RequeueAfter: requeueAfter}, nil
    }

    if !windowResult.InWindow {
        // Outside window, requeue
        requeueAfter := RequeueAfter(windowResult, now)
        return ctrl.Result{RequeueAfter: requeueAfter}, nil
    }

    // Step 6: Safety gates (unchanged)
    // Step 7: Change gates (unchanged)

    // Step 8: Start maintenance (NON-BLOCKING)
    return r.startMaintenance(ctx, policy, rec, currentMemory, windowResult, now)
}
```

**`startMaintenance` (refactored — non-blocking):**

```go
func (r *PostgresMemoryPolicyReconciler) startMaintenance(
    ctx context.Context,
    policy *policyv1alpha1.PostgresMemoryPolicy,
    rec *VPARecommendation,
    currentMemory *resource.Quantity,
    windowResult WindowResult,
    now time.Time,
) (ctrl.Result, error) {
    logger := log.FromContext(ctx)
    logger.Info("starting maintenance", "memoryTarget", rec.Memory.String())

    prevMemoryStr := ""
    if currentMemory != nil {
        prevMemoryStr = currentMemory.String()
    }

    // Calculate PG parameters
    pgParams, err := calculatePGParams(policy, rec.Memory.Value(), cpuCores(rec))
    if err != nil {
        return r.failMaintenance(ctx, policy, fmt.Sprintf("calculating PG parameters: %v", err))
    }

    // Patch Zalando CR
    if err := r.zalandoPatcher.PatchResources(ctx, policy, rec, pgParams); err != nil {
        return r.failMaintenance(ctx, policy, fmt.Sprintf("patching Zalando CR: %v", err))
    }

    // Mark maintenance as in progress with PatchApplied phase
    SetCondition(policy, policyv1alpha1.ConditionMaintenanceInProgress,
        metav1.ConditionTrue, "MaintenanceStarted", "maintenance run has started")
    addMaintenanceRecord(policy, policyv1alpha1.MaintenanceRecord{
        StartedAt:      metav1.Now(),
        Status:         policyv1alpha1.MaintenanceStatusInProgress,
        Phase:          policyv1alpha1.MaintenancePhasePatchApplied,
        PreviousMemory: prevMemoryStr,
        AppliedMemory:  rec.Memory.String(),
    })

    r.Recorder.Eventf(policy, "Normal", "MaintenanceStarted",
        "maintenance started: applying memory=%s to cluster %q",
        rec.Memory.String(), policy.Spec.TargetCluster)

    // Return immediately — do NOT block waiting for cluster ready
    return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}
```

**`monitorMaintenance` (refactored — state machine):**

```go
func (r *PostgresMemoryPolicyReconciler) monitorMaintenance(
    ctx context.Context,
    policy *policyv1alpha1.PostgresMemoryPolicy,
    record *policyv1alpha1.MaintenanceRecord,
    windowResult WindowResult,
    now time.Time,
) (ctrl.Result, error) {
    logger := log.FromContext(ctx)

    switch record.Phase {
    case policyv1alpha1.MaintenancePhasePatchApplied:
        // Phase 1: Waiting for cluster to become Running
        healthy, err := r.zalandoPatcher.IsClusterHealthy(ctx, policy.Namespace, policy.Spec.TargetCluster)
        if err != nil {
            return ctrl.Result{}, fmt.Errorf("checking cluster health: %w", err)
        }
        if !healthy {
            logger.V(1).Info("cluster not yet healthy, waiting")
            return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
        }

        // Cluster is healthy — trigger post-actions
        logger.Info("cluster is healthy, triggering post-actions")
        if len(policy.Spec.PostActions) == 0 {
            // No post-actions — maintenance is complete
            return r.completeMaintenance(ctx, policy, record, windowResult, now)
        }

        if err := r.postActions.TriggerPostActions(ctx, policy); err != nil {
            return r.failMaintenance(ctx, policy, fmt.Sprintf("triggering post-actions: %v", err))
        }

        // Advance phase
        record.Phase = policyv1alpha1.MaintenancePhasePostActionsTriggered
        return ctrl.Result{RequeueAfter: 30 * time.Second}, nil

    case policyv1alpha1.MaintenancePhasePostActionsTriggered:
        // Phase 2: Waiting for post-action rollouts to complete
        done, err := r.postActions.ArePostActionsComplete(ctx, policy)
        if err != nil {
            return ctrl.Result{}, fmt.Errorf("checking post-action completion: %w", err)
        }
        if !done {
            logger.V(1).Info("post-actions not yet complete, waiting")
            return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
        }

        // All done
        return r.completeMaintenance(ctx, policy, record, windowResult, now)

    default:
        // Unknown phase — treat as error, fail the maintenance
        return r.failMaintenance(ctx, policy, fmt.Sprintf("unknown maintenance phase %q", record.Phase))
    }
}
```

**`handleWindowExpired` (new — grace period logic):**

```go
func (r *PostgresMemoryPolicyReconciler) handleWindowExpired(
    ctx context.Context,
    policy *policyv1alpha1.PostgresMemoryPolicy,
    record *policyv1alpha1.MaintenanceRecord,
) (ctrl.Result, error) {
    logger := log.FromContext(ctx)

    // Grace check: if everything is healthy and done, count it as success
    healthy, err := r.zalandoPatcher.IsClusterHealthy(ctx, policy.Namespace, policy.Spec.TargetCluster)
    if err != nil {
        logger.Error(err, "error checking cluster health after window expired")
        return r.failMaintenance(ctx, policy, policyv1alpha1.ReasonMaintenanceTimeout)
    }

    if healthy {
        postActionsDone := true
        if record.Phase == policyv1alpha1.MaintenancePhasePostActionsTriggered {
            postActionsDone, err = r.postActions.ArePostActionsComplete(ctx, policy)
            if err != nil {
                logger.Error(err, "error checking post-actions after window expired")
                return r.failMaintenance(ctx, policy, policyv1alpha1.ReasonMaintenanceTimeout)
            }
        } else if record.Phase == policyv1alpha1.MaintenancePhasePatchApplied {
            // Cluster is healthy but post-actions were never triggered.
            // If there are post-actions defined, we can't count this as complete.
            if len(policy.Spec.PostActions) > 0 {
                postActionsDone = false
            }
        }

        if postActionsDone {
            logger.Info("maintenance completed after window expired (grace period)")
            memTarget := resource.MustParse(record.AppliedMemory)
            policy.Status.CurrentMemory = &memTarget
            markRecordCompleted(policy, policyv1alpha1.MaintenanceStatusCompleted, "completed after window expired")
            SetCondition(policy, policyv1alpha1.ConditionMaintenanceInProgress,
                metav1.ConditionFalse, "MaintenanceCompleted", "maintenance completed (grace period)")
            SetCondition(policy, policyv1alpha1.ConditionLastMaintenanceFailed,
                metav1.ConditionFalse, "MaintenanceSucceeded", "last maintenance run succeeded")
            r.Recorder.Event(policy, "Normal", "MaintenanceCompleted",
                "maintenance completed after window expired (grace period)")
            return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
        }
    }

    // Not healthy or post-actions incomplete — fail (no revert)
    logger.Info("maintenance window expired, cluster not in desired state")
    return r.failMaintenance(ctx, policy, policyv1alpha1.ReasonMaintenanceTimeout)
}
```

**`completeMaintenance` (new — extracted from old startMaintenance tail):**

```go
func (r *PostgresMemoryPolicyReconciler) completeMaintenance(
    ctx context.Context,
    policy *policyv1alpha1.PostgresMemoryPolicy,
    record *policyv1alpha1.MaintenanceRecord,
    windowResult WindowResult,
    now time.Time,
) (ctrl.Result, error) {
    logger := log.FromContext(ctx)

    memTarget := resource.MustParse(record.AppliedMemory)
    policy.Status.CurrentMemory = &memTarget
    markRecordCompleted(policy, policyv1alpha1.MaintenanceStatusCompleted, "")
    SetCondition(policy, policyv1alpha1.ConditionMaintenanceInProgress,
        metav1.ConditionFalse, "MaintenanceCompleted", "maintenance completed successfully")
    SetCondition(policy, policyv1alpha1.ConditionLastMaintenanceFailed,
        metav1.ConditionFalse, "MaintenanceSucceeded", "last maintenance run succeeded")

    r.Recorder.Eventf(policy, "Normal", "MaintenanceCompleted",
        "maintenance completed: applied memory=%s to cluster %q",
        memTarget.String(), policy.Spec.TargetCluster)
    logger.Info("maintenance completed successfully", "appliedMemory", memTarget.String())

    requeueAfter := RequeueAfter(windowResult, time.Now())
    return ctrl.Result{RequeueAfter: requeueAfter}, nil
}
```

**Remove the `maintenanceTimeoutBuffer` constant and the `context.WithTimeout` in startMaintenance** — no longer needed since the reconciler doesn't block.

**Remove or deprecate `WaitForClusterReady`** — it is no longer called from the reconcile path. `IsClusterHealthy` (single-shot check) is used instead. Keep `WaitForClusterReady` only if there are other callers; otherwise delete it.

#### 6. Remove the old blocking `Execute` usage

The old `r.postActions.Execute(ctx, policy)` call in `startMaintenance` is removed. The reconciler now uses `TriggerPostActions` and `ArePostActionsComplete` in `monitorMaintenance`. The `Execute` method can be kept for backward compatibility or removed if nothing else calls it.

#### 7. Update tests

**Unit tests for `conditions.go`** — Add tests for `hasCompletedMaintenanceInWindow`:

```
- Record completed within window → returns true
- Record completed outside window → returns false
- No completed records → returns false
- Multiple records, one completed in window → returns true
- Record failed in window (not completed) → returns false
```

**Unit tests for `postactions.go`** — Add tests for the new split methods:

```
- TriggerPostActions: applies restart annotation without blocking
- ArePostActionsComplete: returns false when rollout in progress
- ArePostActionsComplete: returns true when rollout finished
- ArePostActionsComplete: returns true when no post-actions defined
```

**Integration tests** — The existing integration tests need updating and new scenarios:

```
Scenario: Single update per window
- Create VPA, Zalando CR, Policy with maintenance window open
- Trigger reconcile → patches Zalando CR (Phase=PatchApplied)
- Set cluster status to Running
- Trigger reconcile → triggers post-actions (Phase=PostActionsTriggered)
- Make post-actions complete
- Trigger reconcile → marks Completed
- Change VPA recommendation significantly (still in window)
- Trigger reconcile → should NOT start new maintenance (one-per-window guard)
- Assert: Zalando CR still has the first applied memory, not the new VPA value

Scenario: Operator restart mid-maintenance
- Start maintenance (Phase=PatchApplied)
- Simulate restart: create a new reconciler, reconcile the same CR
- Assert: reconciler picks up from PatchApplied, does not re-patch

Scenario: Window expires with healthy cluster (grace period)
- Start maintenance (Phase=PatchApplied)
- Set cluster to Running, advance to PostActionsTriggered
- Let window expire
- Reconcile → should check health, find everything healthy, mark Completed

Scenario: Window expires with unhealthy cluster
- Start maintenance (Phase=PatchApplied)
- Let window expire while cluster is still not Running
- Reconcile → should mark Failed, no revert

Scenario: No post-actions defined
- Start maintenance with empty postActions list
- Set cluster to Running
- Reconcile → should go directly to Completed (skip PostActionsTriggered phase)
```

#### 8. Regenerate deepcopy

Run:
```bash
cd /home/soukal/GIT/GITHUB/zalando-vertical-autoscaler
go generate ./api/...
```

If deepcopy is not automated, manually add handling for the `Phase` field in `zz_generated.deepcopy.go`. Since `MaintenancePhase` is a `string` typedef, it is copied by value — the existing `DeepCopyInto` for `MaintenanceRecord` should handle it automatically. Verify this after generation.

#### 9. Update CLAUDE.md

In the "Key design decisions" section, add a new subsection:

```markdown
### Non-blocking reconciler with one update per window
The reconciler uses a non-blocking state machine for maintenance. Instead of
blocking the reconcile goroutine waiting for cluster health and post-action
completion, it:
1. Patches the Zalando CR and sets `Phase=PatchApplied` → returns immediately
2. On subsequent reconciles, polls cluster health → when Running, triggers
   post-actions and sets `Phase=PostActionsTriggered`
3. Polls post-action rollout readiness → when done, marks `Completed`

All state is persisted in the CR status (`MaintenanceRecord.Phase`), so an
operator restart safely resumes from the last persisted phase.

Only ONE Zalando CR patch is allowed per maintenance window. After a successful
maintenance, further changes are deferred to the next window — even if VPA
recommendations change mid-window.

If the window expires while maintenance is in progress, the reconciler performs
a single grace check: if the cluster is healthy and post-actions are complete,
it marks the maintenance as Completed. Otherwise it marks it Failed. No revert
is performed — a revert would cause another restart, doubling disruption.
```

Update the "Change gates" subsection to note the one-per-window behavior.

### Behavioral changes summary

| Aspect | Before | After |
|---|---|---|
| Reconcile blocking | `startMaintenance` blocks for up to 1h (WaitForClusterReady + post-actions) | `startMaintenance` returns immediately after patching |
| Updates per window | Unlimited — each reconcile can start a new maintenance if gates pass | ONE patch per window. After Completed, skip until next window |
| Operator restart during maintenance | Loses in-flight state. May re-patch or restart from scratch | Resumes from persisted Phase (PatchApplied or PostActionsTriggered) |
| Post-action execution | Blocking: trigger + wait in one call | Split: trigger on one reconcile, poll completion on subsequent reconciles |
| Window expiry | failMaintenance immediately | Grace check: if healthy + done → Completed; else → Failed |
| Revert on failure | None | None (unchanged — explicit design decision) |

### Known limitations

**Post-action re-trigger on crash between trigger and persist**: If the operator crashes after applying the restart annotation but before persisting `Phase=PostActionsTriggered`, the next reconcile will see `Phase=PatchApplied`, observe the cluster is healthy, and re-trigger post-actions. This applies a new restart annotation timestamp, causing one extra rolling restart. This only happens on operator crash — a rare event — and is acceptable.

### What NOT to do

- Do NOT revert memory on failure — leave the Zalando CR in its current state
- Do NOT add a `WaitForClusterReady` or blocking poll in the reconcile path
- Do NOT allow multiple patches in the same maintenance window
- Do NOT add CPU limits anywhere
- Do NOT change safety gate or change gate logic (thresholds, evaluation)
- Do NOT change the bootstrap/initialMemory path (it already bypasses windows)
- Do NOT store reconciler state in memory — all state must be in the CR status
- Do NOT remove the `WaitForClusterReady` method if integration tests still use it

### Verification

After implementation, run:
```bash
cd /home/soukal/GIT/GITHUB/zalando-vertical-autoscaler
./run-tests.sh
```

All existing tests must continue to pass (with updates for the new flow). New tests must also pass. Also run:
```bash
go vet ./...
helm lint charts/zalando-vertical-autoscaler
```
