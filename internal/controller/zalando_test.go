package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	policyv1alpha1 "github.com/pricefx/zalando-vertical-autoscaler/api/v1alpha1"
)

func TestEvaluateChangeGates(t *testing.T) {
	tests := []struct {
		name        string
		current     string
		target      string
		wantBlocked bool
		wantReason  string
	}{
		{
			name:        "both gates pass - large enough absolute and relative change",
			current:     "32Gi",
			target:      "48Gi", // +16Gi (>5Gi), +50% (>10%)
			wantBlocked: false,
		},
		{
			name:        "absolute gate blocks - diff is exactly 5Gi",
			current:     "32Gi",
			target:      "37Gi", // +5Gi, ~15% — but absolute gate uses > not >=
			wantBlocked: true,
			wantReason:  policyv1alpha1.ReasonChangeGateAbsoluteDiff,
		},
		{
			name:        "absolute gate blocks - diff less than 5Gi",
			current:     "32Gi",
			target:      "34Gi", // +2Gi, ~6%
			wantBlocked: true,
			wantReason:  policyv1alpha1.ReasonChangeGateAbsoluteDiff,
		},
		{
			name:        "relative gate blocks - diff > 5Gi but < 10%",
			current:     "100Gi",
			target:      "106Gi", // +6Gi but only ~6%
			wantBlocked: true,
			wantReason:  policyv1alpha1.ReasonChangeGateRelativeDiff,
		},
		{
			name:        "decrease passes both gates",
			current:     "48Gi",
			target:      "32Gi", // -16Gi, -33%
			wantBlocked: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			current := resource.MustParse(tc.current)
			target := resource.MustParse(tc.target)
			result := EvaluateChangeGates(current, target)
			if result.Blocked != tc.wantBlocked {
				t.Errorf("Blocked=%v, want %v (reason=%s, message=%s)", result.Blocked, tc.wantBlocked, result.Reason, result.Message)
			}
			if tc.wantBlocked && result.Reason != tc.wantReason {
				t.Errorf("Reason=%s, want %s", result.Reason, tc.wantReason)
			}
		})
	}
}

func TestBuildMemoryPatch(t *testing.T) {
	patch := buildMemoryPatch("16Gi", "32Gi")
	spec, ok := patch["spec"].(map[string]interface{})
	if !ok {
		t.Fatal("spec not found in patch")
	}
	resources, ok := spec["resources"].(map[string]interface{})
	if !ok {
		t.Fatal("resources not found in patch spec")
	}
	requests, ok := resources["requests"].(map[string]interface{})
	if !ok {
		t.Fatal("requests not found in patch resources")
	}
	if requests["memory"] != "16Gi" {
		t.Errorf("memory request = %v, want 16Gi", requests["memory"])
	}
	limits, ok := resources["limits"].(map[string]interface{})
	if !ok {
		t.Fatal("limits not found in patch resources")
	}
	if limits["memory"] != "32Gi" {
		t.Errorf("memory limit = %v, want 32Gi", limits["memory"])
	}
}

func TestBuildMemoryCPUPatch(t *testing.T) {
	patch := buildMemoryCPUPatch("16Gi", "32Gi", "2")
	spec := patch["spec"].(map[string]interface{})
	resources := spec["resources"].(map[string]interface{})
	requests := resources["requests"].(map[string]interface{})
	limits := resources["limits"].(map[string]interface{})

	if requests["cpu"] != "2" {
		t.Errorf("cpu request = %v, want 2", requests["cpu"])
	}
	if _, hasCPULimit := limits["cpu"]; hasCPULimit {
		t.Error("limits.cpu should not be set (no CPU limits policy)")
	}
}

func TestOvercommitCalculation(t *testing.T) {
	tests := []struct {
		name        string
		memRequest  string
		overcommit  float64
		wantLimit   int64
	}{
		{
			name:       "overcommit 1 (limits == requests)",
			memRequest: "16Gi",
			overcommit: 1.0,
			wantLimit:  16 * 1024 * 1024 * 1024,
		},
		{
			name:       "overcommit 2",
			memRequest: "16Gi",
			overcommit: 2.0,
			wantLimit:  32 * 1024 * 1024 * 1024,
		},
		{
			name:       "overcommit 1.5",
			memRequest: "8Gi",
			overcommit: 1.5,
			wantLimit:  12 * 1024 * 1024 * 1024,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			memRequest := resource.MustParse(tc.memRequest)
			limitBytes := int64(float64(memRequest.Value()) * tc.overcommit)
			if limitBytes != tc.wantLimit {
				t.Errorf("limit = %d, want %d", limitBytes, tc.wantLimit)
			}
		})
	}
}
