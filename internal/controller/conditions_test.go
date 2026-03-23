package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	policyv1alpha1 "github.com/pricefx/zalando-vertical-autoscaler/api/v1alpha1"
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

func TestHasCompletedMaintenanceInWindow(t *testing.T) {
	windowStart := time.Date(2026, 3, 23, 2, 0, 0, 0, time.UTC)
	windowEnd := time.Date(2026, 3, 23, 3, 0, 0, 0, time.UTC)

	t.Run("completed record within window returns true", func(t *testing.T) {
		policy := &policyv1alpha1.PostgresMemoryPolicy{}
		addMaintenanceRecord(policy, policyv1alpha1.MaintenanceRecord{
			StartedAt: metav1.NewTime(windowStart.Add(10 * time.Minute)),
			Status:    policyv1alpha1.MaintenanceStatusCompleted,
		})
		if !hasCompletedMaintenanceInWindow(policy, windowStart, windowEnd) {
			t.Fatal("expected true for completed record within window")
		}
	})

	t.Run("completed record outside window returns false", func(t *testing.T) {
		policy := &policyv1alpha1.PostgresMemoryPolicy{}
		addMaintenanceRecord(policy, policyv1alpha1.MaintenanceRecord{
			StartedAt: metav1.NewTime(windowStart.Add(-1 * time.Hour)),
			Status:    policyv1alpha1.MaintenanceStatusCompleted,
		})
		if hasCompletedMaintenanceInWindow(policy, windowStart, windowEnd) {
			t.Fatal("expected false for completed record outside window")
		}
	})

	t.Run("no completed records returns false", func(t *testing.T) {
		policy := &policyv1alpha1.PostgresMemoryPolicy{}
		if hasCompletedMaintenanceInWindow(policy, windowStart, windowEnd) {
			t.Fatal("expected false for empty history")
		}
	})

	t.Run("multiple records one completed in window returns true", func(t *testing.T) {
		policy := &policyv1alpha1.PostgresMemoryPolicy{}
		addMaintenanceRecord(policy, policyv1alpha1.MaintenanceRecord{
			StartedAt: metav1.NewTime(windowStart.Add(-2 * time.Hour)),
			Status:    policyv1alpha1.MaintenanceStatusFailed,
		})
		addMaintenanceRecord(policy, policyv1alpha1.MaintenanceRecord{
			StartedAt: metav1.NewTime(windowStart.Add(5 * time.Minute)),
			Status:    policyv1alpha1.MaintenanceStatusCompleted,
		})
		if !hasCompletedMaintenanceInWindow(policy, windowStart, windowEnd) {
			t.Fatal("expected true when one completed record is in window")
		}
	})

	t.Run("failed record in window does not count", func(t *testing.T) {
		policy := &policyv1alpha1.PostgresMemoryPolicy{}
		addMaintenanceRecord(policy, policyv1alpha1.MaintenanceRecord{
			StartedAt: metav1.NewTime(windowStart.Add(10 * time.Minute)),
			Status:    policyv1alpha1.MaintenanceStatusFailed,
		})
		if hasCompletedMaintenanceInWindow(policy, windowStart, windowEnd) {
			t.Fatal("expected false for failed record in window")
		}
	})

	t.Run("completed record at exact window start is included", func(t *testing.T) {
		policy := &policyv1alpha1.PostgresMemoryPolicy{}
		addMaintenanceRecord(policy, policyv1alpha1.MaintenanceRecord{
			StartedAt: metav1.NewTime(windowStart),
			Status:    policyv1alpha1.MaintenanceStatusCompleted,
		})
		if !hasCompletedMaintenanceInWindow(policy, windowStart, windowEnd) {
			t.Fatal("expected true for completed record at exact window start")
		}
	})

	t.Run("completed record at exact window end is excluded", func(t *testing.T) {
		policy := &policyv1alpha1.PostgresMemoryPolicy{}
		addMaintenanceRecord(policy, policyv1alpha1.MaintenanceRecord{
			StartedAt: metav1.NewTime(windowEnd),
			Status:    policyv1alpha1.MaintenanceStatusCompleted,
		})
		if hasCompletedMaintenanceInWindow(policy, windowStart, windowEnd) {
			t.Fatal("expected false for completed record at exact window end")
		}
	})
}
