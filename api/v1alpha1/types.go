// Package v1alpha1 defines the PostgresMemoryPolicy CRD types.
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PostgresMemoryPolicy is the Schema for the postgresmemorypolicies API.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories=all
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetCluster`
// +kubebuilder:printcolumn:name="VPA",type=string,JSONPath=`.spec.vpaName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type PostgresMemoryPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PostgresMemoryPolicySpec   `json:"spec,omitempty"`
	Status PostgresMemoryPolicyStatus `json:"status,omitempty"`
}

// PostgresMemoryPolicySpec defines the desired state of PostgresMemoryPolicy.
type PostgresMemoryPolicySpec struct {
	// TargetCluster is the name of the Zalando postgresql CR in the same namespace.
	TargetCluster string `json:"targetCluster"`

	// VPAName is the name of the VPA object that holds memory recommendations.
	VPAName string `json:"vpaName"`

	// VPAContainerName is the container name to read VPA recommendations from.
	// Defaults to "postgres".
	// +kubebuilder:default=postgres
	VPAContainerName string `json:"vpaContainerName,omitempty"`

	// MemoryMin is the lower bound for memory requests.
	MemoryMin resource.Quantity `json:"memoryMin"`

	// MemoryMax is the upper bound for memory requests.
	MemoryMax resource.Quantity `json:"memoryMax"`

	// Overcommit sets limits = requests * overcommit. Defaults to 1 (limits == requests).
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	Overcommit float64 `json:"overcommit,omitempty"`

	// MemoryBuffer is a percentage added on top of the VPA memory recommendation
	// after clamping to [memoryMin, memoryMax]. For example, a value of 20 increases
	// the recommended memory by 20%. The buffered value is re-clamped to memoryMax
	// so the buffer can never push memory above the configured upper bound.
	// Defaults to 0 (no buffer).
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	MemoryBuffer float64 `json:"memoryBuffer,omitempty"`

	// InitialMemory is the memory value applied to the Zalando CR when it has no
	// spec.resources.requests.memory set. This bootstraps new clusters before VPA
	// has produced a recommendation. Applied immediately, bypassing the maintenance
	// window and change gates. CPU is derived automatically at a 10:1 memory-to-CPU
	// ratio (e.g., 10Gi memory → 1000m CPU), with a minimum of 100m CPU even when
	// the 10:1 ratio would result in a lower value (for example, <1Gi memory → 100m CPU).
	// +optional
	InitialMemory *resource.Quantity `json:"initialMemory,omitempty"`

	// MaintenanceWindow defines when maintenance may be performed.
	MaintenanceWindow MaintenanceWindowSpec `json:"maintenanceWindow"`

	// SafetyGates are evaluated before starting maintenance.
	// +optional
	SafetyGates SafetyGatesSpec `json:"safetyGates,omitempty"`

	// PostActions are executed in order after a successful PG cluster update.
	// +optional
	PostActions []PostActionSpec `json:"postActions,omitempty"`

	// PostgresParameters maps PostgreSQL parameter names to Go template expressions.
	// Templates receive .memory (bytes int64) and .cpu (cores int64) as inputs.
	// Values starting with "{{" are evaluated as Go templates; others are used as-is.
	// Available template functions: div, mul, add, max (all operate on int64).
	// Evaluated parameters are patched into spec.postgresql.parameters on the Zalando CR.
	// Example: shared_buffers: "{{ div (div .memory 3) 8192 }}"
	// +optional
	PostgresParameters map[string]string `json:"postgresParameters,omitempty"`
}

// MaintenanceWindowSpec defines when the operator may perform maintenance.
type MaintenanceWindowSpec struct {
	// Cron is a 5-field cron expression (minute hour dom month dow) defining the
	// start of the maintenance window. Extended syntax is supported:
	//   L  - last day-of-week in month (e.g. "0 20 * * 0L" = last Sunday at 20:00)
	//   #  - nth day-of-week in month  (e.g. "0 20 * * 0#2" = second Sunday at 20:00)
	//   W  - nearest weekday           (e.g. "0 20 15W * *" = weekday nearest 15th at 20:00)
	Cron string `json:"cron"`

	// TimeoutMinutes is the maximum duration of the maintenance window in minutes.
	// +kubebuilder:default=60
	TimeoutMinutes int `json:"timeoutMinutes,omitempty"`
}

// SafetyGatesSpec defines pre-flight checks before starting maintenance.
type SafetyGatesSpec struct {
	// RequireHealthyCluster aborts maintenance if the Zalando cluster is not healthy.
	// +kubebuilder:default=true
	RequireHealthyCluster bool `json:"requireHealthyCluster,omitempty"`

	// MinReadyReplicas is the minimum number of ready replicas required before proceeding.
	// +kubebuilder:default=1
	MinReadyReplicas int32 `json:"minReadyReplicas,omitempty"`
}

// PostActionSpec defines a single action to execute after a successful maintenance run.
type PostActionSpec struct {
	// Action to perform on the target resource.
	// +kubebuilder:validation:Enum=RolloutRestart
	Action PostActionType `json:"action"`

	// Target resource to act upon.
	Target ActionTargetRef `json:"target"`
}

// PostActionType is the action verb, modelled after kubectl imperative commands.
// +kubebuilder:validation:Enum=RolloutRestart
type PostActionType string

const (
	// PostActionRolloutRestart triggers a rolling restart of the target workload,
	// equivalent to `kubectl rollout restart`.
	PostActionRolloutRestart PostActionType = "RolloutRestart"
)

// ActionTargetRef identifies a workload resource to act upon.
type ActionTargetRef struct {
	// Kind of the target resource.
	// +kubebuilder:validation:Enum=Deployment;StatefulSet;DaemonSet
	Kind string `json:"kind"`

	// Name of the target resource.
	Name string `json:"name"`

	// Namespace of the target resource. Defaults to the same namespace as the
	// PostgresMemoryPolicy CR if omitted.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// PostgresMemoryPolicyStatus defines the observed state of PostgresMemoryPolicy.
type PostgresMemoryPolicyStatus struct {
	// MemoryTarget is the VPA recommendation clamped to min/max.
	// +optional
	MemoryTarget *resource.Quantity `json:"memoryTarget,omitempty"`

	// CurrentMemory is the currently applied memory request in the Zalando CR.
	// +optional
	CurrentMemory *resource.Quantity `json:"currentMemory,omitempty"`

	// MaintenanceHistory holds the last 10 maintenance run records.
	// +optional
	MaintenanceHistory []MaintenanceRecord `json:"maintenanceHistory,omitempty"`

	// Conditions holds standard Kubernetes condition types.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// MaintenanceRecord records the outcome of a single maintenance run.
type MaintenanceRecord struct {
	// StartedAt is the time the maintenance run started.
	StartedAt metav1.Time `json:"startedAt"`

	// CompletedAt is the time the maintenance run completed (or failed).
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// Status is the terminal or intermediate status of the run.
	Status MaintenanceStatus `json:"status"`

	// Reason provides a human-readable explanation for the status.
	// +optional
	Reason string `json:"reason,omitempty"`

	// PreviousMemory is the memory value before this maintenance run.
	// +optional
	PreviousMemory string `json:"previousMemory,omitempty"`

	// AppliedMemory is the memory value applied during this run.
	// +optional
	AppliedMemory string `json:"appliedMemory,omitempty"`
}

// MaintenanceStatus describes the phase of a maintenance run.
// +kubebuilder:validation:Enum=Pending;InProgress;Completed;Failed;Skipped
type MaintenanceStatus string

const (
	// MaintenanceStatusPending means maintenance is scheduled but not yet started.
	MaintenanceStatusPending MaintenanceStatus = "Pending"
	// MaintenanceStatusInProgress means maintenance is currently underway.
	MaintenanceStatusInProgress MaintenanceStatus = "InProgress"
	// MaintenanceStatusCompleted means maintenance finished successfully.
	MaintenanceStatusCompleted MaintenanceStatus = "Completed"
	// MaintenanceStatusFailed means maintenance encountered an error.
	MaintenanceStatusFailed MaintenanceStatus = "Failed"
	// MaintenanceStatusSkipped means maintenance was skipped due to a gate or check.
	MaintenanceStatusSkipped MaintenanceStatus = "Skipped"
)

// Condition type constants for PostgresMemoryPolicy.
const (
	// ConditionMaintenanceInProgress indicates maintenance is currently running.
	ConditionMaintenanceInProgress = "MaintenanceInProgress"
	// ConditionLastMaintenanceFailed indicates the most recent maintenance run failed.
	ConditionLastMaintenanceFailed = "LastMaintenanceFailed"
	// ConditionVPARecommendationReady indicates a valid VPA recommendation is available.
	ConditionVPARecommendationReady = "VPARecommendationReady"
)

// Condition reason constants.
const (
	ReasonVPANotFound         = "VPANotFound"
	ReasonNoRecommendationYet = "NoRecommendationYet"
	ReasonClusterUnhealthy    = "ClusterUnhealthy"
	ReasonChangeGateAbsoluteDiff = "ChangeGateAbsoluteDiff"
	ReasonChangeGateRelativeDiff = "ChangeGateRelativeDiff"
	ReasonMaintenanceTimeout  = "MaintenanceTimeout"
	ReasonPostActionFailed    = "PostActionFailed"
)

// PostgresMemoryPolicyList contains a list of PostgresMemoryPolicy.
// +kubebuilder:object:root=true
type PostgresMemoryPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PostgresMemoryPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PostgresMemoryPolicy{}, &PostgresMemoryPolicyList{})
}
