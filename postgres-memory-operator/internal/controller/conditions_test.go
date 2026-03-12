package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	policyv1alpha1 "github.com/pricefx/postgres-memory-operator/api/v1alpha1"
)

func TestSetCondition(t *testing.T) {
	policy := &policyv1alpha1.PostgresMemoryPolicy{}

	// Add a new condition.
	changed := SetCondition(policy, policyv1alpha1.ConditionVPARecommendationReady,
		metav1.ConditionFalse, policyv1alpha1.ReasonVPANotFound, "vpa not found")
	if !changed {
		t.Fatal("expected changed=true for new condition")
	}
	if len(policy.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(policy.Status.Conditions))
	}

	// Same values should not change.
	changed = SetCondition(policy, policyv1alpha1.ConditionVPARecommendationReady,
		metav1.ConditionFalse, policyv1alpha1.ReasonVPANotFound, "vpa not found")
	if changed {
		t.Fatal("expected changed=false for identical condition update")
	}

	// Different status should change.
	changed = SetCondition(policy, policyv1alpha1.ConditionVPARecommendationReady,
		metav1.ConditionTrue, "Ready", "recommendation available")
	if !changed {
		t.Fatal("expected changed=true for status change")
	}
}

func TestIsConditionTrue(t *testing.T) {
	policy := &policyv1alpha1.PostgresMemoryPolicy{}
	if IsConditionTrue(policy, policyv1alpha1.ConditionMaintenanceInProgress) {
		t.Fatal("expected false for missing condition")
	}

	SetCondition(policy, policyv1alpha1.ConditionMaintenanceInProgress,
		metav1.ConditionTrue, "Started", "maintenance started")
	if !IsConditionTrue(policy, policyv1alpha1.ConditionMaintenanceInProgress) {
		t.Fatal("expected true after setting condition to True")
	}
}

func TestAddMaintenanceRecord(t *testing.T) {
	policy := &policyv1alpha1.PostgresMemoryPolicy{}

	for i := 0; i < 12; i++ {
		addMaintenanceRecord(policy, policyv1alpha1.MaintenanceRecord{
			StartedAt: metav1.Now(),
			Status:    policyv1alpha1.MaintenanceStatusCompleted,
		})
	}
	if len(policy.Status.MaintenanceHistory) != 10 {
		t.Fatalf("expected history trimmed to 10, got %d", len(policy.Status.MaintenanceHistory))
	}
}

func TestFindInProgressRecord(t *testing.T) {
	policy := &policyv1alpha1.PostgresMemoryPolicy{}
	addMaintenanceRecord(policy, policyv1alpha1.MaintenanceRecord{
		StartedAt: metav1.Now(),
		Status:    policyv1alpha1.MaintenanceStatusInProgress,
	})

	rec := findInProgressRecord(policy)
	if rec == nil {
		t.Fatal("expected to find in-progress record")
	}

	markRecordCompleted(policy, policyv1alpha1.MaintenanceStatusCompleted, "done")
	if rec.Status != policyv1alpha1.MaintenanceStatusCompleted {
		t.Fatalf("expected Completed status, got %s", rec.Status)
	}
	if rec.CompletedAt == nil {
		t.Fatal("expected CompletedAt to be set")
	}
}
