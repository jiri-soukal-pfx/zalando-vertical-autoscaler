package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	policyv1alpha1 "github.com/pricefx/zalando-vertical-autoscaler/api/v1alpha1"
)

const (
	// rolloutRestartAnnotation is the standard annotation used to trigger a rollout restart.
	rolloutRestartAnnotation = "kubectl.kubernetes.io/restartedAt"
	// rolloutRestartTimeout is the per-target timeout for waiting for rollout completion.
	rolloutRestartTimeout = 10 * time.Minute
)

// PostActionExecutor executes post-maintenance actions.
type PostActionExecutor struct {
	client client.Client
}

// NewPostActionExecutor creates a new PostActionExecutor.
func NewPostActionExecutor(c client.Client) *PostActionExecutor {
	return &PostActionExecutor{client: c}
}

// Execute runs all post-actions defined in the policy. It stops at the first failure.
func (e *PostActionExecutor) Execute(ctx context.Context, policy *policyv1alpha1.PostgresMemoryPolicy) error {
	for _, action := range policy.Spec.PostActions {
		if err := e.dispatch(ctx, policy, action); err != nil {
			return fmt.Errorf("post-action %s on %s/%s: %w", action.Action, action.Target.Kind, action.Target.Name, err)
		}
	}
	return nil
}

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

// trigger applies the post-action trigger without waiting for completion.
func (e *PostActionExecutor) trigger(ctx context.Context, policy *policyv1alpha1.PostgresMemoryPolicy, action policyv1alpha1.PostActionSpec) error {
	switch action.Action {
	case policyv1alpha1.PostActionRolloutRestart:
		return e.triggerRolloutRestart(ctx, policy, action.Target)
	default:
		return fmt.Errorf("unknown post-action type %q", action.Action)
	}
}

// isComplete checks if a single post-action target has finished its rollout.
func (e *PostActionExecutor) isComplete(ctx context.Context, policy *policyv1alpha1.PostgresMemoryPolicy, action policyv1alpha1.PostActionSpec) (bool, error) {
	switch action.Action {
	case policyv1alpha1.PostActionRolloutRestart:
		return e.isRolloutComplete(ctx, policy, action.Target)
	default:
		return false, fmt.Errorf("unknown post-action type %q", action.Action)
	}
}

// triggerRolloutRestart applies the restart annotation without waiting.
func (e *PostActionExecutor) triggerRolloutRestart(ctx context.Context, policy *policyv1alpha1.PostgresMemoryPolicy, target policyv1alpha1.ActionTargetRef) error {
	namespace := target.Namespace
	if namespace == "" {
		namespace = policy.Namespace
	}

	restartedAt := time.Now().UTC().Format(time.RFC3339)
	patch := fmt.Sprintf(
		`{"spec":{"template":{"metadata":{"annotations":{%q:%q}}}}}`,
		rolloutRestartAnnotation, restartedAt,
	)

	switch target.Kind {
	case "Deployment":
		obj := &appsv1.Deployment{}
		if err := e.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: target.Name}, obj); err != nil {
			return fmt.Errorf("getting Deployment %s/%s: %w", namespace, target.Name, err)
		}
		return e.client.Patch(ctx, obj, client.RawPatch(types.StrategicMergePatchType, []byte(patch)))
	case "StatefulSet":
		obj := &appsv1.StatefulSet{}
		if err := e.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: target.Name}, obj); err != nil {
			return fmt.Errorf("getting StatefulSet %s/%s: %w", namespace, target.Name, err)
		}
		return e.client.Patch(ctx, obj, client.RawPatch(types.StrategicMergePatchType, []byte(patch)))
	case "DaemonSet":
		obj := &appsv1.DaemonSet{}
		if err := e.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: target.Name}, obj); err != nil {
			return fmt.Errorf("getting DaemonSet %s/%s: %w", namespace, target.Name, err)
		}
		return e.client.Patch(ctx, obj, client.RawPatch(types.StrategicMergePatchType, []byte(patch)))
	default:
		return fmt.Errorf("unsupported target kind %q", target.Kind)
	}
}

// isRolloutComplete checks if a single rollout restart target is ready.
func (e *PostActionExecutor) isRolloutComplete(ctx context.Context, policy *policyv1alpha1.PostgresMemoryPolicy, target policyv1alpha1.ActionTargetRef) (bool, error) {
	namespace := target.Namespace
	if namespace == "" {
		namespace = policy.Namespace
	}

	switch target.Kind {
	case "Deployment":
		obj := &appsv1.Deployment{}
		if err := e.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: target.Name}, obj); err != nil {
			return false, fmt.Errorf("getting Deployment %s/%s: %w", namespace, target.Name, err)
		}
		desired := int32(1)
		if obj.Spec.Replicas != nil {
			desired = *obj.Spec.Replicas
		}
		return obj.Status.UpdatedReplicas == desired && obj.Status.AvailableReplicas == desired, nil
	case "StatefulSet":
		obj := &appsv1.StatefulSet{}
		if err := e.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: target.Name}, obj); err != nil {
			return false, fmt.Errorf("getting StatefulSet %s/%s: %w", namespace, target.Name, err)
		}
		desired := int32(1)
		if obj.Spec.Replicas != nil {
			desired = *obj.Spec.Replicas
		}
		return obj.Status.UpdatedReplicas == desired && obj.Status.ReadyReplicas == desired, nil
	case "DaemonSet":
		obj := &appsv1.DaemonSet{}
		if err := e.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: target.Name}, obj); err != nil {
			return false, fmt.Errorf("getting DaemonSet %s/%s: %w", namespace, target.Name, err)
		}
		desired := obj.Status.DesiredNumberScheduled
		return obj.Status.UpdatedNumberScheduled == desired && obj.Status.NumberReady == desired, nil
	default:
		return false, fmt.Errorf("unsupported target kind %q", target.Kind)
	}
}

// dispatch selects the handler for the given action type.
func (e *PostActionExecutor) dispatch(ctx context.Context, policy *policyv1alpha1.PostgresMemoryPolicy, action policyv1alpha1.PostActionSpec) error {
	switch action.Action {
	case policyv1alpha1.PostActionRolloutRestart:
		return e.rolloutRestart(ctx, policy, action.Target)
	default:
		return fmt.Errorf("unknown post-action type %q", action.Action)
	}
}

// rolloutRestart triggers a rollout restart on the target workload and waits
// for completion.
func (e *PostActionExecutor) rolloutRestart(ctx context.Context, policy *policyv1alpha1.PostgresMemoryPolicy, target policyv1alpha1.ActionTargetRef) error {
	namespace := target.Namespace
	if namespace == "" {
		namespace = policy.Namespace
	}

	restartedAt := time.Now().UTC().Format(time.RFC3339)
	patch := fmt.Sprintf(
		`{"spec":{"template":{"metadata":{"annotations":{%q:%q}}}}}`,
		rolloutRestartAnnotation, restartedAt,
	)

	switch target.Kind {
	case "Deployment":
		return e.rolloutRestartDeployment(ctx, namespace, target.Name, []byte(patch))
	case "StatefulSet":
		return e.rolloutRestartStatefulSet(ctx, namespace, target.Name, []byte(patch))
	case "DaemonSet":
		return e.rolloutRestartDaemonSet(ctx, namespace, target.Name, []byte(patch))
	default:
		return fmt.Errorf("unsupported target kind %q: must be one of Deployment, StatefulSet, DaemonSet", target.Kind)
	}
}

func (e *PostActionExecutor) rolloutRestartDeployment(ctx context.Context, namespace, name string, patch []byte) error {
	obj := &appsv1.Deployment{}
	if err := e.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, obj); err != nil {
		return fmt.Errorf("getting Deployment %s/%s: %w", namespace, name, err)
	}

	if err := e.client.Patch(ctx, obj, client.RawPatch(types.StrategicMergePatchType, patch)); err != nil {
		return fmt.Errorf("patching Deployment %s/%s: %w", namespace, name, err)
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, rolloutRestartTimeout)
	defer cancel()

	return e.waitDeploymentReady(timeoutCtx, namespace, name)
}

func (e *PostActionExecutor) waitDeploymentReady(ctx context.Context, namespace, name string) error {
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for Deployment %s/%s rollout: %w", namespace, name, ctx.Err())
		default:
		}

		obj := &appsv1.Deployment{}
		if err := e.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, obj); err != nil {
			return fmt.Errorf("polling Deployment %s/%s: %w", namespace, name, err)
		}

		desired := int32(1)
		if obj.Spec.Replicas != nil {
			desired = *obj.Spec.Replicas
		}
		if obj.Status.UpdatedReplicas == desired &&
			obj.Status.AvailableReplicas == desired {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for Deployment %s/%s rollout: %w", namespace, name, ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
}

func (e *PostActionExecutor) rolloutRestartStatefulSet(ctx context.Context, namespace, name string, patch []byte) error {
	obj := &appsv1.StatefulSet{}
	if err := e.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, obj); err != nil {
		return fmt.Errorf("getting StatefulSet %s/%s: %w", namespace, name, err)
	}

	if err := e.client.Patch(ctx, obj, client.RawPatch(types.StrategicMergePatchType, patch)); err != nil {
		return fmt.Errorf("patching StatefulSet %s/%s: %w", namespace, name, err)
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, rolloutRestartTimeout)
	defer cancel()

	return e.waitStatefulSetReady(timeoutCtx, namespace, name)
}

func (e *PostActionExecutor) waitStatefulSetReady(ctx context.Context, namespace, name string) error {
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for StatefulSet %s/%s rollout: %w", namespace, name, ctx.Err())
		default:
		}

		obj := &appsv1.StatefulSet{}
		if err := e.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, obj); err != nil {
			return fmt.Errorf("polling StatefulSet %s/%s: %w", namespace, name, err)
		}

		desired := int32(1)
		if obj.Spec.Replicas != nil {
			desired = *obj.Spec.Replicas
		}
		if obj.Status.UpdatedReplicas == desired &&
			obj.Status.ReadyReplicas == desired {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for StatefulSet %s/%s rollout: %w", namespace, name, ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
}

func (e *PostActionExecutor) rolloutRestartDaemonSet(ctx context.Context, namespace, name string, patch []byte) error {
	obj := &appsv1.DaemonSet{}
	if err := e.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, obj); err != nil {
		return fmt.Errorf("getting DaemonSet %s/%s: %w", namespace, name, err)
	}

	if err := e.client.Patch(ctx, obj, client.RawPatch(types.StrategicMergePatchType, patch)); err != nil {
		return fmt.Errorf("patching DaemonSet %s/%s: %w", namespace, name, err)
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, rolloutRestartTimeout)
	defer cancel()

	return e.waitDaemonSetReady(timeoutCtx, namespace, name)
}

func (e *PostActionExecutor) waitDaemonSetReady(ctx context.Context, namespace, name string) error {
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for DaemonSet %s/%s rollout: %w", namespace, name, ctx.Err())
		default:
		}

		obj := &appsv1.DaemonSet{}
		if err := e.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, obj); err != nil {
			return fmt.Errorf("polling DaemonSet %s/%s: %w", namespace, name, err)
		}

		desired := obj.Status.DesiredNumberScheduled
		if obj.Status.UpdatedNumberScheduled == desired &&
			obj.Status.NumberReady == desired {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for DaemonSet %s/%s rollout: %w", namespace, name, ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
}
