// Package controller implements the PostgresMemoryPolicy reconciler.
package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	policyv1alpha1 "github.com/pricefx/zalando-vertical-autoscaler/api/v1alpha1"
)


// PostgresMemoryPolicyReconciler reconciles PostgresMemoryPolicy objects.
type PostgresMemoryPolicyReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	windowEvaluator *WindowEvaluator
	vpaReader       *VPAReader
	zalandoPatcher  *ZalandoPatcher
	postActions     *PostActionExecutor
}

// NewPostgresMemoryPolicyReconciler creates a new reconciler with all dependencies.
func NewPostgresMemoryPolicyReconciler(c client.Client, scheme *runtime.Scheme, recorder record.EventRecorder) *PostgresMemoryPolicyReconciler {
	return &PostgresMemoryPolicyReconciler{
		Client:          c,
		Scheme:          scheme,
		Recorder:        recorder,
		windowEvaluator: NewWindowEvaluator(),
		vpaReader:       NewVPAReader(c),
		zalandoPatcher:  NewZalandoPatcher(c),
		postActions:     NewPostActionExecutor(c),
	}
}

// +kubebuilder:rbac:groups=pricefx.io,resources=postgresmemorypolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pricefx.io,resources=postgresmemorypolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pricefx.io,resources=postgresmemorypolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=autoscaling.k8s.io,resources=verticalpodautoscalers,verbs=get;list;watch
// +kubebuilder:rbac:groups=acid.zalan.do,resources=postgresqls,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is the main reconcile loop for PostgresMemoryPolicy.
func (r *PostgresMemoryPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	policy := &policyv1alpha1.PostgresMemoryPolicy{}
	if err := r.Get(ctx, req.NamespacedName, policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Take a snapshot for status patching.
	patchBase := client.MergeFrom(policy.DeepCopy())

	result, err := r.reconcilePolicy(ctx, policy)

	// Always persist status changes.
	if statusErr := r.Status().Patch(ctx, policy, patchBase); statusErr != nil {
		logger.Error(statusErr, "failed to patch status")
		if err == nil {
			err = statusErr
		}
	}

	if err != nil {
		return ctrl.Result{}, err
	}
	return result, nil
}

// reconcilePolicy contains the core business logic, separated from status patching.
func (r *PostgresMemoryPolicyReconciler) reconcilePolicy(ctx context.Context, policy *policyv1alpha1.PostgresMemoryPolicy) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Step 1: Bootstrap — if the Zalando CR has no memory set and InitialMemory
	// is configured, apply initial resources immediately (no window check, no change gates).
	currentMemory, err := r.zalandoPatcher.GetCurrentMemory(ctx, policy.Namespace, policy.Spec.TargetCluster)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reading current memory for bootstrap check: %w", err)
	}

	if currentMemory == nil && policy.Spec.InitialMemory != nil {
		logger.Info("Zalando CR has no memory set, applying initialMemory", "initialMemory", policy.Spec.InitialMemory.String())

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

		pgParams, err := calculatePGParams(policy, memBytes, cpuCores(initialRec))
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("calculating PG parameters for bootstrap: %w", err)
		}

		if err := r.zalandoPatcher.PatchResources(ctx, policy, initialRec, pgParams); err != nil {
			return ctrl.Result{}, fmt.Errorf("applying initial memory: %w", err)
		}

		policy.Status.CurrentMemory = &initialMemory
		r.Recorder.Eventf(policy, "Normal", "InitialMemoryApplied",
			"applied initial memory=%s cpu=%s to cluster %q (no prior resources set)",
			initialMemory.String(), cpuQuantity.String(), policy.Spec.TargetCluster)

		return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
	}

	// Step 2: Sync VPA recommendation.
	rec, err := r.vpaReader.ReadRecommendation(ctx, policy)
	if err != nil {
		recErr, ok := err.(*RecommendationError)
		if ok {
			SetCondition(policy, policyv1alpha1.ConditionVPARecommendationReady,
				metav1.ConditionFalse, recErr.Reason, recErr.Message)
			r.Recorder.Event(policy, "Warning", recErr.Reason, recErr.Message)
			return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
		}
		return ctrl.Result{}, fmt.Errorf("reading VPA recommendation: %w", err)
	}

	SetCondition(policy, policyv1alpha1.ConditionVPARecommendationReady,
		metav1.ConditionTrue, "Ready", "VPA recommendation is available")

	memTarget := rec.Memory
	policy.Status.MemoryTarget = &memTarget

	// Step 2: Evaluate maintenance window.
	now := time.Now()
	windowResult, err := r.windowEvaluator.Evaluate(
		policy.Spec.MaintenanceWindow.Cron,
		policy.Spec.MaintenanceWindow.TimeoutMinutes,
		now,
	)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("evaluating maintenance window: %w", err)
	}

	// Step 3: Check if maintenance is already in progress.
	if IsConditionTrue(policy, policyv1alpha1.ConditionMaintenanceInProgress) {
		inProgressRec := findInProgressRecord(policy)
		if inProgressRec != nil {
			if !windowResult.InWindow {
				// Window expired — check for grace period success.
				logger.Info("maintenance window expired while maintenance in progress")
				return r.handleWindowExpired(ctx, policy, inProgressRec)
			}
			// Still in window — advance phases.
			logger.Info("maintenance already in progress, monitoring")
			return r.monitorMaintenance(ctx, policy, inProgressRec, windowResult, now)
		}
		// Stale condition with no matching record — clear it.
		SetCondition(policy, policyv1alpha1.ConditionMaintenanceInProgress,
			metav1.ConditionFalse, "StaleConditionCleared", "no in-progress record found")
	}

	// Step 3b: One-update-per-window guard.
	if windowResult.InWindow && hasCompletedMaintenanceInWindow(policy, windowResult.NextOpen, windowResult.WindowEnd) {
		logger.V(1).Info("maintenance already completed in this window, skipping")
		requeueAfter := RequeueAfter(windowResult, now)
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	if !windowResult.InWindow {
		logger.V(1).Info("outside maintenance window", "nextOpen", windowResult.NextOpen)
		requeueAfter := RequeueAfter(windowResult, now)
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	// Step 4: Safety gates.
	if policy.Spec.SafetyGates.RequireHealthyCluster {
		healthy, err := r.zalandoPatcher.IsClusterHealthy(ctx, policy.Namespace, policy.Spec.TargetCluster)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("checking cluster health: %w", err)
		}
		if !healthy {
			msg := fmt.Sprintf("cluster %q is not healthy; skipping maintenance", policy.Spec.TargetCluster)
			logger.Info(msg)
			r.Recorder.Event(policy, "Warning", policyv1alpha1.ReasonClusterUnhealthy, msg)
			addMaintenanceRecord(policy, policyv1alpha1.MaintenanceRecord{
				StartedAt: metav1.Now(),
				Status:    policyv1alpha1.MaintenanceStatusSkipped,
				Reason:    policyv1alpha1.ReasonClusterUnhealthy,
			})
			requeueAfter := RequeueAfter(windowResult, now)
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
		}
	}

	// Step 5: Change gates (reuse currentMemory from bootstrap check above).
	if currentMemory != nil {
		policy.Status.CurrentMemory = currentMemory
		gateResult := EvaluateChangeGates(*currentMemory, memTarget, &policy.Spec.SafetyGates)
		if gateResult.Blocked {
			logger.Info("change gate blocked maintenance", "reason", gateResult.Reason, "message", gateResult.Message)
			r.Recorder.Event(policy, "Normal", gateResult.Reason, gateResult.Message)
			addMaintenanceRecord(policy, policyv1alpha1.MaintenanceRecord{
				StartedAt: metav1.Now(),
				Status:    policyv1alpha1.MaintenanceStatusSkipped,
				Reason:    gateResult.Reason,
			})
			SetCondition(policy, policyv1alpha1.ConditionMaintenanceInProgress,
				metav1.ConditionFalse, gateResult.Reason, gateResult.Message)
			requeueAfter := RequeueAfter(windowResult, now)
			return ctrl.Result{RequeueAfter: requeueAfter}, nil
		}
	}

	// Step 6: Start maintenance.
	return r.startMaintenance(ctx, policy, rec, currentMemory, windowResult, now)
}

// startMaintenance begins a maintenance run (non-blocking).
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

	// Calculate PG parameters from templates.
	pgParams, err := calculatePGParams(policy, rec.Memory.Value(), cpuCores(rec))
	if err != nil {
		return r.failMaintenance(ctx, policy, fmt.Sprintf("calculating PG parameters: %v", err))
	}

	// Patch Zalando CR.
	if err := r.zalandoPatcher.PatchResources(ctx, policy, rec, pgParams); err != nil {
		return r.failMaintenance(ctx, policy, fmt.Sprintf("patching Zalando CR: %v", err))
	}

	// Mark maintenance as in progress with PatchApplied phase.
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

	// Return immediately — do NOT block waiting for cluster ready.
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// monitorMaintenance advances the non-blocking state machine for an in-progress maintenance.
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
		// Phase 1: Waiting for cluster to become Running.
		healthy, err := r.zalandoPatcher.IsClusterHealthy(ctx, policy.Namespace, policy.Spec.TargetCluster)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("checking cluster health: %w", err)
		}
		if !healthy {
			logger.V(1).Info("cluster not yet healthy, waiting")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		// Cluster is healthy — trigger post-actions or complete.
		logger.Info("cluster is healthy, triggering post-actions")
		if len(policy.Spec.PostActions) == 0 {
			return r.completeMaintenance(ctx, policy, record, windowResult, now)
		}

		if err := r.postActions.TriggerPostActions(ctx, policy); err != nil {
			return r.failMaintenance(ctx, policy, fmt.Sprintf("triggering post-actions: %v", err))
		}

		// Advance phase.
		record.Phase = policyv1alpha1.MaintenancePhasePostActionsTriggered
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil

	case policyv1alpha1.MaintenancePhasePostActionsTriggered:
		// Phase 2: Waiting for post-action rollouts to complete.
		done, err := r.postActions.ArePostActionsComplete(ctx, policy)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("checking post-action completion: %w", err)
		}
		if !done {
			logger.V(1).Info("post-actions not yet complete, waiting")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		return r.completeMaintenance(ctx, policy, record, windowResult, now)

	default:
		return r.failMaintenance(ctx, policy, fmt.Sprintf("unknown maintenance phase %q", record.Phase))
	}
}

// handleWindowExpired performs a grace-period check when the window expires during maintenance.
func (r *PostgresMemoryPolicyReconciler) handleWindowExpired(
	ctx context.Context,
	policy *policyv1alpha1.PostgresMemoryPolicy,
	record *policyv1alpha1.MaintenanceRecord,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Grace check: if everything is healthy and done, count it as success.
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

	// Not healthy or post-actions incomplete — fail (no revert).
	logger.Info("maintenance window expired, cluster not in desired state")
	return r.failMaintenance(ctx, policy, policyv1alpha1.ReasonMaintenanceTimeout)
}

// completeMaintenance marks a maintenance run as successfully completed.
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

// failMaintenance records a failed maintenance run and sets the appropriate conditions.
func (r *PostgresMemoryPolicyReconciler) failMaintenance(
	ctx context.Context,
	policy *policyv1alpha1.PostgresMemoryPolicy,
	reason string,
) (ctrl.Result, error) {
	log.FromContext(ctx).Error(nil, "maintenance failed", "reason", reason)

	markRecordCompleted(policy, policyv1alpha1.MaintenanceStatusFailed, reason)
	SetCondition(policy, policyv1alpha1.ConditionMaintenanceInProgress,
		metav1.ConditionFalse, "MaintenanceFailed", reason)
	SetCondition(policy, policyv1alpha1.ConditionLastMaintenanceFailed,
		metav1.ConditionTrue, "MaintenanceFailed", reason)

	r.Recorder.Event(policy, "Warning", "MaintenanceFailed", reason)

	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// calculatePGParams evaluates PostgresParameters templates if defined on the policy.
// Returns nil if no parameters are configured.
func calculatePGParams(policy *policyv1alpha1.PostgresMemoryPolicy, memoryBytes int64, cpu int64) (map[string]string, error) {
	if len(policy.Spec.PostgresParameters) == 0 {
		return nil, nil
	}
	return CalculatePostgresParameters(policy.Spec.PostgresParameters, memoryBytes, cpu)
}

// cpuCores returns the CPU value from a VPA recommendation in whole cores.
// Returns 0 if CPU is not set.
func cpuCores(rec *VPARecommendation) int64 {
	if rec.CPU == nil {
		return 0
	}
	// MilliValue() returns milliCPU; divide by 1000 to get cores.
	// Use ceiling to round up partial cores (e.g. 3200m → 4 cores),
	// matching the helm chart's round_up_to_cores behavior.
	millis := rec.CPU.MilliValue()
	cores := millis / 1000
	if millis%1000 > 0 {
		cores++
	}
	return cores
}

// SetupWithManager registers the controller with the manager.
func (r *PostgresMemoryPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&policyv1alpha1.PostgresMemoryPolicy{}).
		Complete(r)
}
