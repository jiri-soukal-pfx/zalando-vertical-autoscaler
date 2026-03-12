package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	policyv1alpha1 "github.com/pricefx/postgres-memory-operator/api/v1alpha1"
)

const (
	// zalandoGroup is the API group for the Zalando postgres operator.
	zalandoGroup = "acid.zalan.do"
	// zalandoVersion is the API version for the Zalando postgres CR.
	zalandoVersion = "v1"
	// zalandoKind is the kind of the Zalando postgres CR.
	zalandoKind = "postgresql"
	// changeGateAbsoluteThreshold is the minimum absolute memory change (in bytes)
	// required to proceed with maintenance.
	changeGateAbsoluteThreshold = 5 * 1024 * 1024 * 1024 // 5Gi

	// changeGateRelativeThreshold is the minimum relative memory change (as a fraction)
	// required to proceed with maintenance.
	changeGateRelativeThreshold = 0.10 // 10%
)

// ZalandoPatcher patches Zalando postgresql CRs with new memory/CPU values.
type ZalandoPatcher struct {
	client client.Client
}

// NewZalandoPatcher creates a new ZalandoPatcher.
func NewZalandoPatcher(c client.Client) *ZalandoPatcher {
	return &ZalandoPatcher{client: c}
}

// ChangeGateResult describes why a change gate blocked the update.
type ChangeGateResult struct {
	// Blocked is true if the change gate blocked the update.
	Blocked bool
	// Reason is the machine-readable reason (matches a policyv1alpha1.Reason* constant).
	Reason string
	// Message is a human-readable explanation.
	Message string
}

// EvaluateChangeGates checks if the difference between current and target memory is
// large enough to justify a maintenance run. Both the absolute and relative thresholds
// must be met. If either is not met, the gate blocks.
func EvaluateChangeGates(current, target resource.Quantity) ChangeGateResult {
	currentBytes := current.Value()
	targetBytes := target.Value()

	absDiff := int64(math.Abs(float64(targetBytes - currentBytes)))
	if absDiff <= changeGateAbsoluteThreshold {
		return ChangeGateResult{
			Blocked: true,
			Reason:  policyv1alpha1.ReasonChangeGateAbsoluteDiff,
			Message: fmt.Sprintf("absolute memory diff %s does not exceed threshold of 5Gi", formatBytes(absDiff)),
		}
	}

	if currentBytes == 0 {
		return ChangeGateResult{Blocked: false}
	}
	relativeDiff := math.Abs(float64(targetBytes-currentBytes)) / float64(currentBytes)
	if relativeDiff <= changeGateRelativeThreshold {
		return ChangeGateResult{
			Blocked: true,
			Reason:  policyv1alpha1.ReasonChangeGateRelativeDiff,
			Message: fmt.Sprintf("relative memory diff %.1f%% does not exceed threshold of 10%%", relativeDiff*100),
		}
	}

	return ChangeGateResult{Blocked: false}
}

// GetCurrentMemory reads the current memory request from the Zalando postgresql CR.
func (p *ZalandoPatcher) GetCurrentMemory(ctx context.Context, namespace, name string) (*resource.Quantity, error) {
	pg, err := p.getPostgresql(ctx, namespace, name)
	if err != nil {
		return nil, err
	}

	memStr, found, err := unstructured.NestedString(pg.Object, "spec", "resources", "requests", "memory")
	if err != nil || !found || memStr == "" {
		return nil, nil
	}
	q := resource.MustParse(memStr)
	return &q, nil
}

// PatchResources patches the Zalando postgresql CR with new memory and optionally CPU values.
func (p *ZalandoPatcher) PatchResources(ctx context.Context, policy *policyv1alpha1.PostgresMemoryPolicy, rec *VPARecommendation) error {
	memRequest := rec.Memory
	overcommit := policy.Spec.Overcommit
	if overcommit < 1 {
		overcommit = 1
	}
	memLimitBytes := int64(float64(memRequest.Value()) * overcommit)
	memLimit := resource.NewQuantity(memLimitBytes, resource.BinarySI)

	patchData := buildMemoryPatch(memRequest.String(), memLimit.String())

	if rec.CPU != nil {
		cpuLimitMillis := int64(float64(rec.CPU.MilliValue()) * overcommit)
		cpuLimit := resource.NewMilliQuantity(cpuLimitMillis, resource.DecimalSI)
		patchData = buildMemoryCPUPatch(memRequest.String(), memLimit.String(), rec.CPU.String(), cpuLimit.String())
	}

	raw, err := json.Marshal(patchData)
	if err != nil {
		return fmt.Errorf("marshalling patch for %s/%s: %w", policy.Namespace, policy.Spec.TargetCluster, err)
	}

	pg := &unstructured.Unstructured{}
	pg.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   zalandoGroup,
		Version: zalandoVersion,
		Kind:    zalandoKind,
	})
	pg.SetName(policy.Spec.TargetCluster)
	pg.SetNamespace(policy.Namespace)

	if err := p.client.Patch(ctx, pg, client.RawPatch(types.MergePatchType, raw)); err != nil {
		return fmt.Errorf("patching postgresql %s/%s: %w", policy.Namespace, policy.Spec.TargetCluster, err)
	}
	return nil
}

func (p *ZalandoPatcher) getPostgresql(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	pg := &unstructured.Unstructured{}
	pg.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   zalandoGroup,
		Version: zalandoVersion,
		Kind:    zalandoKind,
	})
	err := p.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, pg)
	if err != nil {
		return nil, fmt.Errorf("getting postgresql %s/%s: %w", namespace, name, err)
	}
	return pg, nil
}

// WaitForClusterReady polls the Zalando CR until the cluster status is "Running"
// or the context deadline is exceeded.
func (p *ZalandoPatcher) WaitForClusterReady(ctx context.Context, namespace, name string) error {
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for cluster %s/%s to become ready: %w", namespace, name, ctx.Err())
		default:
		}

		pg, err := p.getPostgresql(ctx, namespace, name)
		if err != nil {
			return err
		}

		state, _, _ := unstructured.NestedString(pg.Object, "status", "PostgresClusterStatus")
		if state == "Running" {
			return nil
		}
	}
}

// IsClusterHealthy checks whether the Zalando cluster status indicates a healthy cluster.
func (p *ZalandoPatcher) IsClusterHealthy(ctx context.Context, namespace, name string) (bool, error) {
	pg, err := p.getPostgresql(ctx, namespace, name)
	if err != nil {
		return false, err
	}

	state, _, _ := unstructured.NestedString(pg.Object, "status", "PostgresClusterStatus")
	return state == "Running", nil
}

// buildMemoryPatch constructs a merge-patch map for memory-only resources.
func buildMemoryPatch(memRequest, memLimit string) map[string]interface{} {
	return map[string]interface{}{
		"spec": map[string]interface{}{
			"resources": map[string]interface{}{
				"requests": map[string]interface{}{
					"memory": memRequest,
				},
				"limits": map[string]interface{}{
					"memory": memLimit,
				},
			},
		},
	}
}

// buildMemoryCPUPatch constructs a merge-patch map for memory and CPU resources.
func buildMemoryCPUPatch(memRequest, memLimit, cpuRequest, cpuLimit string) map[string]interface{} {
	return map[string]interface{}{
		"spec": map[string]interface{}{
			"resources": map[string]interface{}{
				"requests": map[string]interface{}{
					"memory": memRequest,
					"cpu":    cpuRequest,
				},
				"limits": map[string]interface{}{
					"memory": memLimit,
					"cpu":    cpuLimit,
				},
			},
		},
	}
}

// formatBytes formats an int64 byte count as a human-readable string.
func formatBytes(b int64) string {
	const gib = 1024 * 1024 * 1024
	if b >= gib {
		return fmt.Sprintf("%.1fGi", float64(b)/gib)
	}
	return fmt.Sprintf("%dB", b)
}
