package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	vpav1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	policyv1alpha1 "github.com/pricefx/zalando-vertical-autoscaler/api/v1alpha1"
)

// VPARecommendation holds the memory and CPU target read from a VPA object.
type VPARecommendation struct {
	// Memory is the target memory request (clamped to min/max).
	Memory resource.Quantity
	// CPU is the target CPU request from the VPA recommendation.
	CPU *resource.Quantity
}

// VPAReader reads recommendations from VPA objects.
type VPAReader struct {
	client client.Client
}

// NewVPAReader creates a new VPAReader.
func NewVPAReader(c client.Client) *VPAReader {
	return &VPAReader{client: c}
}

// ReadRecommendation fetches the VPA object and returns the clamped memory target.
// It returns a non-nil error only for unexpected failures; for expected conditions
// (VPA not found, no recommendation yet), it returns a descriptive RecommendationError.
func (r *VPAReader) ReadRecommendation(ctx context.Context, policy *policyv1alpha1.PostgresMemoryPolicy) (*VPARecommendation, error) {
	vpa := &vpav1.VerticalPodAutoscaler{}
	err := r.client.Get(ctx, types.NamespacedName{
		Namespace: policy.Namespace,
		Name:      policy.Spec.VPAName,
	}, vpa)
	if err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, &RecommendationError{Reason: policyv1alpha1.ReasonVPANotFound, Message: fmt.Sprintf("VPA %q not found in namespace %q", policy.Spec.VPAName, policy.Namespace)}
		}
		return nil, fmt.Errorf("getting VPA %q: %w", policy.Spec.VPAName, err)
	}

	containerName := policy.Spec.VPAContainerName
	if containerName == "" {
		containerName = "postgres"
	}

	rec, err := extractContainerRecommendation(vpa, containerName)
	if err != nil {
		return nil, err
	}

	clampedMemory := clampQuantity(rec.Memory, policy.Spec.MemoryMin, policy.Spec.MemoryMax)
	bufferedMemory := applyMemoryBuffer(clampedMemory, policy.Spec.MemoryBuffer, policy.Spec.MemoryMax)
	return &VPARecommendation{
		Memory: bufferedMemory,
		CPU:    rec.CPU,
	}, nil
}

// containerRecommendation is an intermediate struct for a single container.
type containerRecommendation struct {
	Memory resource.Quantity
	CPU    *resource.Quantity
}

func extractContainerRecommendation(vpa *vpav1.VerticalPodAutoscaler, containerName string) (*containerRecommendation, error) {
	if vpa.Status.Recommendation == nil || len(vpa.Status.Recommendation.ContainerRecommendations) == 0 {
		return nil, &RecommendationError{
			Reason:  policyv1alpha1.ReasonNoRecommendationYet,
			Message: fmt.Sprintf("VPA %q has no recommendation yet", vpa.Name),
		}
	}

	for _, cr := range vpa.Status.Recommendation.ContainerRecommendations {
		if cr.ContainerName != containerName {
			continue
		}
		memTarget, ok := cr.Target[corev1.ResourceMemory]
		if !ok {
			return nil, &RecommendationError{
				Reason:  policyv1alpha1.ReasonNoRecommendationYet,
				Message: fmt.Sprintf("VPA %q has no memory recommendation for container %q", vpa.Name, containerName),
			}
		}
		rec := &containerRecommendation{
			Memory: memTarget.DeepCopy(),
		}
		if cpuTarget, ok := cr.Target[corev1.ResourceCPU]; ok {
			cpu := cpuTarget.DeepCopy()
			rec.CPU = &cpu
		}
		return rec, nil
	}

	return nil, &RecommendationError{
		Reason:  policyv1alpha1.ReasonNoRecommendationYet,
		Message: fmt.Sprintf("VPA %q has no recommendation for container %q", vpa.Name, containerName),
	}
}

// applyMemoryBuffer increases the memory quantity by bufferPercent and re-clamps
// to max so the buffer can never push memory above the configured upper bound.
// A bufferPercent of 0 returns the original quantity unchanged.
func applyMemoryBuffer(q resource.Quantity, bufferPercent float64, max resource.Quantity) resource.Quantity {
	if bufferPercent <= 0 {
		return q
	}
	bufferedBytes := int64(float64(q.Value()) * (1 + bufferPercent/100))
	buffered := *resource.NewQuantity(bufferedBytes, resource.BinarySI)
	if buffered.Cmp(max) > 0 {
		return max.DeepCopy()
	}
	return buffered
}

// clampQuantity clamps q to [min, max].
func clampQuantity(q, min, max resource.Quantity) resource.Quantity {
	if q.Cmp(min) < 0 {
		return min.DeepCopy()
	}
	if q.Cmp(max) > 0 {
		return max.DeepCopy()
	}
	return q.DeepCopy()
}

// RecommendationError is a typed error for expected VPA recommendation failures.
type RecommendationError struct {
	Reason  string
	Message string
}

func (e *RecommendationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Reason, e.Message)
}

// IsRecommendationError returns true if err is a *RecommendationError.
func IsRecommendationError(err error) bool {
	_, ok := err.(*RecommendationError)
	return ok
}
