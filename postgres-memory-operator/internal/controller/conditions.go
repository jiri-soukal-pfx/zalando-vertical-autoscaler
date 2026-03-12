package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	policyv1alpha1 "github.com/pricefx/postgres-memory-operator/api/v1alpha1"
)

// SetCondition sets or updates a condition on the policy's status. It returns true
// if the condition was changed.
func SetCondition(policy *policyv1alpha1.PostgresMemoryPolicy, condType string, status metav1.ConditionStatus, reason, message string) bool {
	now := metav1.Now()
	existing := findCondition(policy.Status.Conditions, condType)
	if existing != nil &&
		existing.Status == status &&
		existing.Reason == reason &&
		existing.Message == message {
		return false
	}

	newCond := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
		ObservedGeneration: policy.Generation,
	}

	if existing == nil {
		policy.Status.Conditions = append(policy.Status.Conditions, newCond)
	} else {
		if existing.Status != status {
			existing.LastTransitionTime = now
		}
		existing.Status = status
		existing.Reason = reason
		existing.Message = message
		existing.ObservedGeneration = policy.Generation
	}
	return true
}

// IsConditionTrue returns true if the given condition type is present and True.
func IsConditionTrue(policy *policyv1alpha1.PostgresMemoryPolicy, condType string) bool {
	c := findCondition(policy.Status.Conditions, condType)
	return c != nil && c.Status == metav1.ConditionTrue
}

// findCondition returns a pointer to the condition with the given type, or nil.
func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}

// addMaintenanceRecord appends a record to the history and trims to the last 10 entries.
func addMaintenanceRecord(policy *policyv1alpha1.PostgresMemoryPolicy, record policyv1alpha1.MaintenanceRecord) {
	policy.Status.MaintenanceHistory = append(policy.Status.MaintenanceHistory, record)
	const maxHistory = 10
	if len(policy.Status.MaintenanceHistory) > maxHistory {
		policy.Status.MaintenanceHistory = policy.Status.MaintenanceHistory[len(policy.Status.MaintenanceHistory)-maxHistory:]
	}
}

// findInProgressRecord returns a pointer to the first InProgress record, or nil.
func findInProgressRecord(policy *policyv1alpha1.PostgresMemoryPolicy) *policyv1alpha1.MaintenanceRecord {
	for i := range policy.Status.MaintenanceHistory {
		if policy.Status.MaintenanceHistory[i].Status == policyv1alpha1.MaintenanceStatusInProgress {
			return &policy.Status.MaintenanceHistory[i]
		}
	}
	return nil
}

// markRecordCompleted marks the in-progress record as completed with the given status.
func markRecordCompleted(policy *policyv1alpha1.PostgresMemoryPolicy, status policyv1alpha1.MaintenanceStatus, reason string) {
	rec := findInProgressRecord(policy)
	if rec == nil {
		return
	}
	now := metav1.Now()
	rec.Status = status
	rec.Reason = reason
	rec.CompletedAt = &now
}
