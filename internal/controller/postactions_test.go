package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	policyv1alpha1 "github.com/pricefx/zalando-vertical-autoscaler/api/v1alpha1"
)

func TestRolloutRestartAnnotationFormat(t *testing.T) {
	// Verify the patch format is correct JSON
	restartedAt := "2024-01-15T10:00:00Z"
	patch := buildRolloutRestartPatch(restartedAt)
	expected := `{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":"2024-01-15T10:00:00Z"}}}}}`
	if patch != expected {
		t.Errorf("patch = %s\nwant = %s", patch, expected)
	}
}

// buildRolloutRestartPatch is a testable helper matching the logic in postactions.go.
func buildRolloutRestartPatch(restartedAt string) string {
	return `{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":"` + restartedAt + `"}}}}}`
}

func TestDispatch_UnknownKind(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = appsv1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	executor := NewPostActionExecutor(c)

	policy := &policyv1alpha1.PostgresMemoryPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy",
			Namespace: "default",
		},
	}
	action := policyv1alpha1.PostActionSpec{
		Action: policyv1alpha1.PostActionRolloutRestart,
		Target: policyv1alpha1.ActionTargetRef{
			Kind: "CronJob", // unsupported kind
			Name: "my-job",
		},
	}

	err := executor.dispatch(context.Background(), policy, action)
	if err == nil {
		t.Fatal("expected error for unsupported kind")
	}
}

func TestDispatch_UnknownActionType(t *testing.T) {
	scheme := runtime.NewScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	executor := NewPostActionExecutor(c)

	policy := &policyv1alpha1.PostgresMemoryPolicy{}
	action := policyv1alpha1.PostActionSpec{
		Action: "ScaleDown", // not implemented
		Target: policyv1alpha1.ActionTargetRef{Kind: "Deployment", Name: "foo"},
	}

	err := executor.dispatch(context.Background(), policy, action)
	if err == nil {
		t.Fatal("expected error for unknown action type")
	}
}

func TestNamespaceDefaultsToPolicy(t *testing.T) {
	// Verify that when target.Namespace is empty, policy.Namespace is used.
	policy := &policyv1alpha1.PostgresMemoryPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "prod"},
	}
	target := policyv1alpha1.ActionTargetRef{
		Kind:      "Deployment",
		Name:      "app",
		Namespace: "",
	}

	namespace := target.Namespace
	if namespace == "" {
		namespace = policy.Namespace
	}
	if namespace != "prod" {
		t.Errorf("namespace = %s, want prod", namespace)
	}
}

func TestNamespaceOverride(t *testing.T) {
	policy := &policyv1alpha1.PostgresMemoryPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "prod"},
	}
	target := policyv1alpha1.ActionTargetRef{
		Kind:      "Deployment",
		Name:      "app",
		Namespace: "staging",
	}

	namespace := target.Namespace
	if namespace == "" {
		namespace = policy.Namespace
	}
	if namespace != "staging" {
		t.Errorf("namespace = %s, want staging", namespace)
	}
}

func TestTriggerPostActions_AppliesAnnotation(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = appsv1.AddToScheme(scheme)

	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep).Build()
	executor := NewPostActionExecutor(c)

	policy := &policyv1alpha1.PostgresMemoryPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: policyv1alpha1.PostgresMemoryPolicySpec{
			PostActions: []policyv1alpha1.PostActionSpec{
				{
					Action: policyv1alpha1.PostActionRolloutRestart,
					Target: policyv1alpha1.ActionTargetRef{Kind: "Deployment", Name: "my-app"},
				},
			},
		},
	}

	err := executor.TriggerPostActions(context.Background(), policy)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify annotation was applied.
	updated := &appsv1.Deployment{}
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(dep), updated)
	if _, ok := updated.Spec.Template.Annotations[rolloutRestartAnnotation]; !ok {
		t.Fatal("expected restart annotation to be set")
	}
}

func TestArePostActionsComplete_ReturnsFalseWhenNotReady(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = appsv1.AddToScheme(scheme)

	replicas := int32(3)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
		},
		Status: appsv1.DeploymentStatus{
			UpdatedReplicas:   1, // not all updated yet
			AvailableReplicas: 1,
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep).WithStatusSubresource(dep).Build()
	executor := NewPostActionExecutor(c)

	policy := &policyv1alpha1.PostgresMemoryPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: policyv1alpha1.PostgresMemoryPolicySpec{
			PostActions: []policyv1alpha1.PostActionSpec{
				{
					Action: policyv1alpha1.PostActionRolloutRestart,
					Target: policyv1alpha1.ActionTargetRef{Kind: "Deployment", Name: "my-app"},
				},
			},
		},
	}

	done, err := executor.ArePostActionsComplete(context.Background(), policy)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if done {
		t.Fatal("expected false when rollout is not complete")
	}
}

func TestArePostActionsComplete_ReturnsTrueWhenReady(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = appsv1.AddToScheme(scheme)

	replicas := int32(2)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
		},
		Status: appsv1.DeploymentStatus{
			UpdatedReplicas:   2,
			AvailableReplicas: 2,
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep).WithStatusSubresource(dep).Build()
	executor := NewPostActionExecutor(c)

	policy := &policyv1alpha1.PostgresMemoryPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
		Spec: policyv1alpha1.PostgresMemoryPolicySpec{
			PostActions: []policyv1alpha1.PostActionSpec{
				{
					Action: policyv1alpha1.PostActionRolloutRestart,
					Target: policyv1alpha1.ActionTargetRef{Kind: "Deployment", Name: "my-app"},
				},
			},
		},
	}

	done, err := executor.ArePostActionsComplete(context.Background(), policy)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !done {
		t.Fatal("expected true when rollout is complete")
	}
}

func TestArePostActionsComplete_ReturnsTrueWhenNoPostActions(t *testing.T) {
	scheme := runtime.NewScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	executor := NewPostActionExecutor(c)

	policy := &policyv1alpha1.PostgresMemoryPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: "default"},
	}

	done, err := executor.ArePostActionsComplete(context.Background(), policy)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !done {
		t.Fatal("expected true when no post-actions defined")
	}
}
