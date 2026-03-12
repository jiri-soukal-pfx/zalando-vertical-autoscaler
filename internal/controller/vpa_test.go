package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	vpav1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"

	policyv1alpha1 "github.com/pricefx/zalando-vertical-autoscaler/api/v1alpha1"
)

func TestClampQuantity(t *testing.T) {
	tests := []struct {
		name     string
		q        string
		min      string
		max      string
		expected string
	}{
		{
			name:     "below min",
			q:        "4Gi",
			min:      "8Gi",
			max:      "32Gi",
			expected: "8Gi",
		},
		{
			name:     "above max",
			q:        "64Gi",
			min:      "8Gi",
			max:      "32Gi",
			expected: "32Gi",
		},
		{
			name:     "within bounds",
			q:        "16Gi",
			min:      "8Gi",
			max:      "32Gi",
			expected: "16Gi",
		},
		{
			name:     "exactly at min",
			q:        "8Gi",
			min:      "8Gi",
			max:      "32Gi",
			expected: "8Gi",
		},
		{
			name:     "exactly at max",
			q:        "32Gi",
			min:      "8Gi",
			max:      "32Gi",
			expected: "32Gi",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := resource.MustParse(tc.q)
			min := resource.MustParse(tc.min)
			max := resource.MustParse(tc.max)
			result := clampQuantity(q, min, max)
			expected := resource.MustParse(tc.expected)
			if result.Cmp(expected) != 0 {
				t.Errorf("clampQuantity(%s, %s, %s) = %s, want %s", tc.q, tc.min, tc.max, result.String(), tc.expected)
			}
		})
	}
}

func TestExtractContainerRecommendation_NoStatus(t *testing.T) {
	vpa := &vpav1.VerticalPodAutoscaler{}
	vpa.Name = "test-vpa"
	_, err := extractContainerRecommendation(vpa, "postgres")
	if err == nil {
		t.Fatal("expected error for nil recommendation status")
	}
	recErr, ok := err.(*RecommendationError)
	if !ok {
		t.Fatalf("expected RecommendationError, got %T", err)
	}
	if recErr.Reason != policyv1alpha1.ReasonNoRecommendationYet {
		t.Errorf("expected reason %s, got %s", policyv1alpha1.ReasonNoRecommendationYet, recErr.Reason)
	}
}

func TestExtractContainerRecommendation_ContainerNotFound(t *testing.T) {
	vpa := &vpav1.VerticalPodAutoscaler{}
	vpa.Name = "test-vpa"
	vpa.Status.Recommendation = &vpav1.RecommendedPodResources{
		ContainerRecommendations: []vpav1.RecommendedContainerResources{
			{ContainerName: "sidecar"},
		},
	}
	_, err := extractContainerRecommendation(vpa, "postgres")
	if err == nil {
		t.Fatal("expected error for missing container")
	}
}

func TestExtractContainerRecommendation_Success(t *testing.T) {
	memTarget := resource.MustParse("16Gi")
	cpuTarget := resource.MustParse("2")
	vpa := &vpav1.VerticalPodAutoscaler{}
	vpa.Name = "test-vpa"
	vpa.Status.Recommendation = &vpav1.RecommendedPodResources{
		ContainerRecommendations: []vpav1.RecommendedContainerResources{
			{
				ContainerName: "postgres",
				Target: corev1.ResourceList{
					corev1.ResourceMemory: memTarget,
					corev1.ResourceCPU:    cpuTarget,
				},
			},
		},
	}

	rec, err := extractContainerRecommendation(vpa, "postgres")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Memory.Cmp(memTarget) != 0 {
		t.Errorf("memory = %s, want %s", rec.Memory.String(), memTarget.String())
	}
	if rec.CPU == nil {
		t.Fatal("expected CPU recommendation")
	}
	if rec.CPU.Cmp(cpuTarget) != 0 {
		t.Errorf("cpu = %s, want %s", rec.CPU.String(), cpuTarget.String())
	}
}
