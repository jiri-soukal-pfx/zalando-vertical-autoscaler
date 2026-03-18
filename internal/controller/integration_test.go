package controller_test

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	vpav1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	policyv1alpha1 "github.com/pricefx/zalando-vertical-autoscaler/api/v1alpha1"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// cronAlwaysOpen is a cron expression that fires every minute. Combined with a
// 1440-minute timeout, the window is always considered open during tests.
const cronAlwaysOpen = "* * * * *"

// cronNeverOpen fires only on January 1st at midnight – reliably outside the
// window for any test that runs after that date.
const cronNeverOpen = "0 0 1 1 *"

// reconcilePolicy calls Reconcile for the given policy and returns the result.
func reconcilePolicy(ctx context.Context, policy *policyv1alpha1.PostgresMemoryPolicy) (ctrl.Result, error) {
	return reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      policy.Name,
			Namespace: policy.Namespace,
		},
	})
}

// makeVPA creates a VPA in the given namespace with a target memory recommendation
// for the "postgres" container. cpuMillis may be 0 to omit the CPU recommendation.
func makeVPA(ctx context.Context, namespace, name, memoryTarget string, cpuMillis int64) *vpav1.VerticalPodAutoscaler {
	vpa := &vpav1.VerticalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: vpav1.VerticalPodAutoscalerSpec{},
	}
	Expect(k8sClient.Create(ctx, vpa)).To(Succeed())

	// VPA recommendations live in the status subresource.
	target := corev1.ResourceList{
		corev1.ResourceMemory: resource.MustParse(memoryTarget),
	}
	if cpuMillis > 0 {
		target[corev1.ResourceCPU] = *resource.NewMilliQuantity(cpuMillis, resource.DecimalSI)
	}
	vpa.Status = vpav1.VerticalPodAutoscalerStatus{
		Recommendation: &vpav1.RecommendedPodResources{
			ContainerRecommendations: []vpav1.RecommendedContainerResources{
				{
					ContainerName: "postgres",
					Target:        target,
				},
			},
		},
	}
	Expect(k8sClient.Status().Update(ctx, vpa)).To(Succeed())
	return vpa
}

// zalandoGVK is the GroupVersionKind for the Zalando postgresql CR used in tests.
var zalandoGVK = schema.GroupVersionKind{Group: "acid.zalan.do", Version: "v1", Kind: "postgresql"}

// makeZalandoCluster creates a Zalando postgresql CR with the given current
// memory request and sets its status.PostgresClusterStatus to statusValue.
func makeZalandoCluster(ctx context.Context, namespace, name, currentMemory, statusValue string) *unstructured.Unstructured {
	pg := &unstructured.Unstructured{}
	pg.SetGroupVersionKind(zalandoGVK)
	pg.SetName(name)
	pg.SetNamespace(namespace)

	spec := map[string]interface{}{
		"resources": map[string]interface{}{
			"requests": map[string]interface{}{
				"memory": currentMemory,
			},
		},
	}
	Expect(unstructured.SetNestedField(pg.Object, spec, "spec")).To(Succeed())
	Expect(k8sClient.Create(ctx, pg)).To(Succeed())

	// Status is a separate subresource.
	pg.Object["status"] = map[string]interface{}{
		"PostgresClusterStatus": statusValue,
	}
	Expect(k8sClient.Status().Update(ctx, pg)).To(Succeed())
	return pg
}

// makePolicy creates a PostgresMemoryPolicy with sensible defaults.
// callers can override fields after construction if needed.
func makePolicy(ctx context.Context, namespace, name, vpaName, targetCluster, cronExpr string) *policyv1alpha1.PostgresMemoryPolicy {
	policy := &policyv1alpha1.PostgresMemoryPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: policyv1alpha1.PostgresMemoryPolicySpec{
			TargetCluster: targetCluster,
			VPAName:       vpaName,
			MemoryMin:     resource.MustParse("8Gi"),
			MemoryMax:     resource.MustParse("64Gi"),
			Overcommit:    1,
			MaintenanceWindow: policyv1alpha1.MaintenanceWindowSpec{
				Cron:           cronExpr,
				TimeoutMinutes: 1440, // 24 h — window effectively always open for cronAlwaysOpen
			},
			SafetyGates: policyv1alpha1.SafetyGatesSpec{
				RequireHealthyCluster: true,
			},
		},
	}
	Expect(k8sClient.Create(ctx, policy)).To(Succeed())
	return policy
}

// getPolicy re-fetches the policy from the API server.
func getPolicy(ctx context.Context, policy *policyv1alpha1.PostgresMemoryPolicy) *policyv1alpha1.PostgresMemoryPolicy {
	updated := &policyv1alpha1.PostgresMemoryPolicy{}
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(policy), updated)).To(Succeed())
	return updated
}

// findCondition returns the condition with the given type from the slice, or nil.
func findConditionInSlice(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}

// makeDeploymentWithZeroReplicas creates a Deployment whose desired replica count
// is 0, so the readiness check in postactions.go passes immediately (0 == 0).
func makeDeploymentWithZeroReplicas(ctx context.Context, namespace, name string) *appsv1.Deployment {
	replicas := int32(0)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": name},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "busybox"},
					},
				},
			},
		},
	}
	Expect(k8sClient.Create(ctx, dep)).To(Succeed())
	return dep
}

// uniqueName returns a unique resource name prefixed with prefix.
var nameCounter int

func uniqueName(prefix string) string {
	nameCounter++
	return fmt.Sprintf("%s-%d", prefix, nameCounter)
}

// makeZalandoClusterNoResources creates a Zalando postgresql CR with no
// spec.resources block and sets its status.PostgresClusterStatus to statusValue.
func makeZalandoClusterNoResources(ctx context.Context, namespace, name, statusValue string) *unstructured.Unstructured {
	pg := &unstructured.Unstructured{}
	pg.SetGroupVersionKind(zalandoGVK)
	pg.SetName(name)
	pg.SetNamespace(namespace)
	Expect(unstructured.SetNestedField(pg.Object, map[string]interface{}{}, "spec")).To(Succeed())
	Expect(k8sClient.Create(ctx, pg)).To(Succeed())

	pg.Object["status"] = map[string]interface{}{
		"PostgresClusterStatus": statusValue,
	}
	Expect(k8sClient.Status().Update(ctx, pg)).To(Succeed())
	return pg
}

// ── test suite ────────────────────────────────────────────────────────────────

var _ = Describe("PostgresMemoryPolicy Reconciler", func() {
	var (
		ctx context.Context
		ns  *corev1.Namespace
	)

	BeforeEach(func() {
		ctx = context.Background()
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-ns-",
			},
		}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, ns)).To(Succeed())
	})

	// ── Scenario 1: VPA not found ─────────────────────────────────────────────

	It("sets VPARecommendationReady=False with reason VPANotFound when the VPA does not exist", func() {
		policy := makePolicy(ctx, ns.Name, uniqueName("policy"), "nonexistent-vpa", "pg-cluster", cronAlwaysOpen)

		result, err := reconcilePolicy(ctx, policy)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(5 * time.Minute))

		updated := getPolicy(ctx, policy)
		cond := findConditionInSlice(updated.Status.Conditions, policyv1alpha1.ConditionVPARecommendationReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(policyv1alpha1.ReasonVPANotFound))
		Expect(updated.Status.MaintenanceHistory).To(BeEmpty())
	})

	// ── Scenario 2: VPA exists but has no recommendation ─────────────────────

	It("sets VPARecommendationReady=False with reason NoRecommendationYet when VPA status is empty", func() {
		vpaName := uniqueName("vpa")
		vpa := &vpav1.VerticalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: vpaName, Namespace: ns.Name},
			Spec:       vpav1.VerticalPodAutoscalerSpec{},
		}
		Expect(k8sClient.Create(ctx, vpa)).To(Succeed())
		// Deliberately do not set any status recommendation.

		policy := makePolicy(ctx, ns.Name, uniqueName("policy"), vpaName, "pg-cluster", cronAlwaysOpen)

		result, err := reconcilePolicy(ctx, policy)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(5 * time.Minute))

		updated := getPolicy(ctx, policy)
		cond := findConditionInSlice(updated.Status.Conditions, policyv1alpha1.ConditionVPARecommendationReady)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(policyv1alpha1.ReasonNoRecommendationYet))
		Expect(updated.Status.MaintenanceHistory).To(BeEmpty())
	})

	// ── Scenario 3: Outside maintenance window ────────────────────────────────

	It("does not start maintenance and requeues when outside the maintenance window", func() {
		vpaName := uniqueName("vpa")
		pgName := uniqueName("pg")
		makeVPA(ctx, ns.Name, vpaName, "20Gi", 0)
		makeZalandoCluster(ctx, ns.Name, pgName, "1Gi", "Running")

		policy := makePolicy(ctx, ns.Name, uniqueName("policy"), vpaName, pgName, cronNeverOpen)

		result, err := reconcilePolicy(ctx, policy)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))

		updated := getPolicy(ctx, policy)
		// VPA recommendation must have been read successfully.
		vpaReady := findConditionInSlice(updated.Status.Conditions, policyv1alpha1.ConditionVPARecommendationReady)
		Expect(vpaReady).NotTo(BeNil())
		Expect(vpaReady.Status).To(Equal(metav1.ConditionTrue))
		// memoryTarget is set, clamped to policy max of 64Gi (target 20Gi is within bounds).
		Expect(updated.Status.MemoryTarget).NotTo(BeNil())
		// No maintenance run should have been started.
		Expect(updated.Status.MaintenanceHistory).To(BeEmpty())
	})

	// ── Scenario 4: Inside window, cluster unhealthy ──────────────────────────

	It("creates a Skipped record when requireHealthyCluster=true and cluster is unhealthy", func() {
		vpaName := uniqueName("vpa")
		pgName := uniqueName("pg")
		makeVPA(ctx, ns.Name, vpaName, "20Gi", 0)
		// Cluster status is "SomeError", not "Running".
		makeZalandoCluster(ctx, ns.Name, pgName, "1Gi", "SomeError")

		policy := makePolicy(ctx, ns.Name, uniqueName("policy"), vpaName, pgName, cronAlwaysOpen)

		result, err := reconcilePolicy(ctx, policy)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))

		updated := getPolicy(ctx, policy)
		Expect(updated.Status.MaintenanceHistory).To(HaveLen(1))
		Expect(updated.Status.MaintenanceHistory[0].Status).To(Equal(policyv1alpha1.MaintenanceStatusSkipped))
		Expect(updated.Status.MaintenanceHistory[0].Reason).To(Equal(policyv1alpha1.ReasonClusterUnhealthy))
	})

	// ── Scenario 5a: Inside window, absolute change gate blocks ──────────────

	It("creates a Skipped record when absolute memory diff is below 5Gi threshold", func() {
		vpaName := uniqueName("vpa")
		pgName := uniqueName("pg")
		// target = 10Gi (within min=8Gi, max=64Gi), current = 10.1Gi → diff ≈ 100Mi < 5Gi.
		makeVPA(ctx, ns.Name, vpaName, "10Gi", 0)
		makeZalandoCluster(ctx, ns.Name, pgName, "10368Mi", "Running") // 10.125Gi ≈ 10Gi + ~100Mi

		policy := makePolicy(ctx, ns.Name, uniqueName("policy"), vpaName, pgName, cronAlwaysOpen)

		_, err := reconcilePolicy(ctx, policy)

		Expect(err).NotTo(HaveOccurred())

		updated := getPolicy(ctx, policy)
		Expect(updated.Status.MaintenanceHistory).To(HaveLen(1))
		Expect(updated.Status.MaintenanceHistory[0].Status).To(Equal(policyv1alpha1.MaintenanceStatusSkipped))
		Expect(updated.Status.MaintenanceHistory[0].Reason).To(Equal(policyv1alpha1.ReasonChangeGateAbsoluteDiff))
	})

	// ── Scenario 5b: Inside window, relative change gate blocks ──────────────

	It("creates a Skipped record when relative memory diff is below 10% threshold", func() {
		vpaName := uniqueName("vpa")
		pgName := uniqueName("pg")
		// current = 100Gi, target = 107Gi → diff = 7Gi > 5Gi (absolute OK),
		// but 7/100 = 7% < 10% (relative blocks).
		policy := makePolicy(ctx, ns.Name, uniqueName("policy"), vpaName, pgName, cronAlwaysOpen)
		// Override min/max to accommodate large values.
		policy.Spec.MemoryMin = resource.MustParse("1Gi")
		policy.Spec.MemoryMax = resource.MustParse("200Gi")
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())

		makeVPA(ctx, ns.Name, vpaName, "107Gi", 0)
		makeZalandoCluster(ctx, ns.Name, pgName, "100Gi", "Running")

		_, err := reconcilePolicy(ctx, policy)

		Expect(err).NotTo(HaveOccurred())

		updated := getPolicy(ctx, policy)
		Expect(updated.Status.MaintenanceHistory).To(HaveLen(1))
		Expect(updated.Status.MaintenanceHistory[0].Status).To(Equal(policyv1alpha1.MaintenanceStatusSkipped))
		Expect(updated.Status.MaintenanceHistory[0].Reason).To(Equal(policyv1alpha1.ReasonChangeGateRelativeDiff))
	})

	// ── Scenario 6: Inside window, healthy cluster – happy path ──────────────

	It("completes maintenance and updates status.currentMemory when all gates pass", func() {
		vpaName := uniqueName("vpa")
		pgName := uniqueName("pg")
		// target = 20Gi (within min=8Gi, max=64Gi), current = 1Gi
		// diff = 19Gi > 5Gi (absolute ✓), 19/1 = 1900% > 10% (relative ✓).
		makeVPA(ctx, ns.Name, vpaName, "20Gi", 0)
		makeZalandoCluster(ctx, ns.Name, pgName, "1Gi", "Running")

		policy := makePolicy(ctx, ns.Name, uniqueName("policy"), vpaName, pgName, cronAlwaysOpen)

		result, err := reconcilePolicy(ctx, policy)

		Expect(err).NotTo(HaveOccurred())
		// Requeue until next window, which is soon (every minute cron).
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))

		updated := getPolicy(ctx, policy)
		Expect(updated.Status.MaintenanceHistory).To(HaveLen(1))
		Expect(updated.Status.MaintenanceHistory[0].Status).To(Equal(policyv1alpha1.MaintenanceStatusCompleted))
		Expect(updated.Status.MaintenanceHistory[0].AppliedMemory).To(Equal("20Gi"))
		Expect(updated.Status.CurrentMemory).NotTo(BeNil())
		Expect(updated.Status.CurrentMemory.String()).To(Equal("20Gi"))

		inProgress := findConditionInSlice(updated.Status.Conditions, policyv1alpha1.ConditionMaintenanceInProgress)
		Expect(inProgress).NotTo(BeNil())
		Expect(inProgress.Status).To(Equal(metav1.ConditionFalse))

		lastFailed := findConditionInSlice(updated.Status.Conditions, policyv1alpha1.ConditionLastMaintenanceFailed)
		Expect(lastFailed).NotTo(BeNil())
		Expect(lastFailed.Status).To(Equal(metav1.ConditionFalse))
	})

	// ── Scenario 7: Post-actions succeed (RolloutRestart) ────────────────────

	It("completes maintenance and applies RolloutRestart to the target Deployment", func() {
		vpaName := uniqueName("vpa")
		pgName := uniqueName("pg")
		depName := uniqueName("app")
		makeVPA(ctx, ns.Name, vpaName, "20Gi", 0)
		makeZalandoCluster(ctx, ns.Name, pgName, "1Gi", "Running")
		makeDeploymentWithZeroReplicas(ctx, ns.Name, depName)

		policy := makePolicy(ctx, ns.Name, uniqueName("policy"), vpaName, pgName, cronAlwaysOpen)
		policy.Spec.PostActions = []policyv1alpha1.PostActionSpec{
			{
				Action: policyv1alpha1.PostActionRolloutRestart,
				Target: policyv1alpha1.ActionTargetRef{
					Kind: "Deployment",
					Name: depName,
					// Namespace intentionally omitted — should default to policy.Namespace.
				},
			},
		}
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())

		_, err := reconcilePolicy(ctx, policy)

		Expect(err).NotTo(HaveOccurred())

		updated := getPolicy(ctx, policy)
		Expect(updated.Status.MaintenanceHistory).To(HaveLen(1))
		Expect(updated.Status.MaintenanceHistory[0].Status).To(Equal(policyv1alpha1.MaintenanceStatusCompleted))

		// Verify the rollout-restart annotation was applied to the Deployment.
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: depName, Namespace: ns.Name}, dep)).To(Succeed())
		Expect(dep.Spec.Template.Annotations).To(HaveKey("kubectl.kubernetes.io/restartedAt"))
	})

	// ── Scenario 8a: Overlapping maintenance run – window still open ──────────

	It("monitors without starting a new run when maintenance is already in progress", func() {
		vpaName := uniqueName("vpa")
		pgName := uniqueName("pg")
		makeVPA(ctx, ns.Name, vpaName, "20Gi", 0)
		makeZalandoCluster(ctx, ns.Name, pgName, "1Gi", "Running")

		policy := makePolicy(ctx, ns.Name, uniqueName("policy"), vpaName, pgName, cronAlwaysOpen)

		// Simulate a previous reconcile that started maintenance by pre-setting the
		// InProgress condition and a corresponding history record.
		policyLatest := getPolicy(ctx, policy)
		policyLatest.Status.Conditions = []metav1.Condition{
			{
				Type:               policyv1alpha1.ConditionMaintenanceInProgress,
				Status:             metav1.ConditionTrue,
				Reason:             "MaintenanceStarted",
				Message:            "in progress",
				LastTransitionTime: metav1.Now(),
			},
		}
		policyLatest.Status.MaintenanceHistory = []policyv1alpha1.MaintenanceRecord{
			{
				StartedAt: metav1.Now(),
				Status:    policyv1alpha1.MaintenanceStatusInProgress,
			},
		}
		Expect(k8sClient.Status().Update(ctx, policyLatest)).To(Succeed())

		result, err := reconcilePolicy(ctx, policy)

		Expect(err).NotTo(HaveOccurred())
		// monitorMaintenance should requeue in 30s while the window is still open.
		Expect(result.RequeueAfter).To(Equal(30 * time.Second))

		updated := getPolicy(ctx, policy)
		// No additional history record should have been appended.
		Expect(updated.Status.MaintenanceHistory).To(HaveLen(1))
		Expect(updated.Status.MaintenanceHistory[0].Status).To(Equal(policyv1alpha1.MaintenanceStatusInProgress))
	})

	// ── Scenario 8b: Maintenance timeout – window expired while in progress ───

	It("records a Failed maintenance run when the window expires while maintenance is in progress", func() {
		vpaName := uniqueName("vpa")
		pgName := uniqueName("pg")
		makeVPA(ctx, ns.Name, vpaName, "20Gi", 0)
		makeZalandoCluster(ctx, ns.Name, pgName, "1Gi", "Running")

		// cronNeverOpen (Jan 1st midnight) with timeout=1 min means the window closed
		// hours/months ago from the perspective of the test.
		policy := makePolicy(ctx, ns.Name, uniqueName("policy"), vpaName, pgName, cronNeverOpen)
		policy.Spec.MaintenanceWindow.TimeoutMinutes = 1
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())

		// Simulate a previous run that got stuck in InProgress while the window was open.
		policyLatest := getPolicy(ctx, policy)
		policyLatest.Status.Conditions = []metav1.Condition{
			{
				Type:               policyv1alpha1.ConditionMaintenanceInProgress,
				Status:             metav1.ConditionTrue,
				Reason:             "MaintenanceStarted",
				Message:            "in progress",
				LastTransitionTime: metav1.Now(),
			},
		}
		policyLatest.Status.MaintenanceHistory = []policyv1alpha1.MaintenanceRecord{
			{
				StartedAt: metav1.Now(),
				Status:    policyv1alpha1.MaintenanceStatusInProgress,
			},
		}
		Expect(k8sClient.Status().Update(ctx, policyLatest)).To(Succeed())

		result, err := reconcilePolicy(ctx, policy)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(5 * time.Minute))

		updated := getPolicy(ctx, policy)
		// The InProgress record must have been marked Failed.
		Expect(updated.Status.MaintenanceHistory).To(HaveLen(1))
		Expect(updated.Status.MaintenanceHistory[0].Status).To(Equal(policyv1alpha1.MaintenanceStatusFailed))
		Expect(updated.Status.MaintenanceHistory[0].Reason).To(Equal(policyv1alpha1.ReasonMaintenanceTimeout))

		lastFailed := findConditionInSlice(updated.Status.Conditions, policyv1alpha1.ConditionLastMaintenanceFailed)
		Expect(lastFailed).NotTo(BeNil())
		Expect(lastFailed.Status).To(Equal(metav1.ConditionTrue))
	})

	// ── Scenario 9: InitialMemory applied when Zalando CR has no resources ───

	It("applies initialMemory when the Zalando CR has no resources set", func() {
		vpaName := uniqueName("vpa")
		pgName := uniqueName("pg")
		makeVPA(ctx, ns.Name, vpaName, "20Gi", 0)
		makeZalandoClusterNoResources(ctx, ns.Name, pgName, "Running")

		initialMem := resource.MustParse("8Gi")
		policy := makePolicy(ctx, ns.Name, uniqueName("policy"), vpaName, pgName, cronNeverOpen)
		policy.Spec.InitialMemory = &initialMem
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())

		result, err := reconcilePolicy(ctx, policy)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(1 * time.Minute))

		// Verify the Zalando CR was patched with initial resources.
		pg := &unstructured.Unstructured{}
		pg.SetGroupVersionKind(zalandoGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pgName, Namespace: ns.Name}, pg)).To(Succeed())

		memReq, _, _ := unstructured.NestedString(pg.Object, "spec", "resources", "requests", "memory")
		Expect(memReq).To(Equal("8Gi"))

		memLimit, _, _ := unstructured.NestedString(pg.Object, "spec", "resources", "limits", "memory")
		Expect(memLimit).To(Equal("8Gi")) // overcommit=1

		cpuReq, _, _ := unstructured.NestedString(pg.Object, "spec", "resources", "requests", "cpu")
		Expect(cpuReq).To(Equal("800m")) // 8Gi → 800m at 10:1 ratio

		// No maintenance history — this is bootstrap, not maintenance.
		updated := getPolicy(ctx, policy)
		Expect(updated.Status.MaintenanceHistory).To(BeEmpty())
		Expect(updated.Status.CurrentMemory).NotTo(BeNil())
		Expect(updated.Status.CurrentMemory.String()).To(Equal("8Gi"))
	})

	It("applies initialMemory when the VPA does not yet exist", func() {
		vpaName := uniqueName("vpa")
		pgName := uniqueName("pg")

		// No VPA is created here on purpose to simulate 'VPA not ready yet'.
		makeZalandoClusterNoResources(ctx, ns.Name, pgName, "Running")

		initialMem := resource.MustParse("8Gi")
		policy := makePolicy(ctx, ns.Name, uniqueName("policy"), vpaName, pgName, cronNeverOpen)
		policy.Spec.InitialMemory = &initialMem
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())

		result, err := reconcilePolicy(ctx, policy)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(1 * time.Minute))

		// Verify the Zalando CR was patched with initial resources.
		pg := &unstructured.Unstructured{}
		pg.SetGroupVersionKind(zalandoGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pgName, Namespace: ns.Name}, pg)).To(Succeed())

		memReq, _, _ := unstructured.NestedString(pg.Object, "spec", "resources", "requests", "memory")
		Expect(memReq).To(Equal("8Gi"))

		memLimit, _, _ := unstructured.NestedString(pg.Object, "spec", "resources", "limits", "memory")
		Expect(memLimit).To(Equal("8Gi")) // overcommit=1

		cpuReq, _, _ := unstructured.NestedString(pg.Object, "spec", "resources", "requests", "cpu")
		Expect(cpuReq).To(Equal("800m")) // 8Gi → 800m at 10:1 ratio

		// No maintenance history — this is bootstrap, not maintenance.
		updated := getPolicy(ctx, policy)
		Expect(updated.Status.MaintenanceHistory).To(BeEmpty())
		Expect(updated.Status.CurrentMemory).NotTo(BeNil())
		Expect(updated.Status.CurrentMemory.String()).To(Equal("8Gi"))
	})

	It("applies initialMemory when the VPA exists but has no recommendation", func() {
		vpaName := uniqueName("vpa")
		pgName := uniqueName("pg")

		// Create a VPA object without a status.recommendation to simulate VPA not ready.
		vpa := &vpav1.VerticalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name:      vpaName,
				Namespace: ns.Name,
			},
			Spec: vpav1.VerticalPodAutoscalerSpec{
				TargetRef: &autoscalingv1.CrossVersionObjectReference{
					APIVersion: zalandoGVK.GroupVersion().String(),
					Kind:       zalandoGVK.Kind,
					Name:       pgName,
				},
			},
		}
		Expect(k8sClient.Create(ctx, vpa)).To(Succeed())

		makeZalandoClusterNoResources(ctx, ns.Name, pgName, "Running")

		initialMem := resource.MustParse("8Gi")
		policy := makePolicy(ctx, ns.Name, uniqueName("policy"), vpaName, pgName, cronNeverOpen)
		policy.Spec.InitialMemory = &initialMem
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())

		result, err := reconcilePolicy(ctx, policy)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(1 * time.Minute))

		// Verify the Zalando CR was patched with initial resources.
		pg := &unstructured.Unstructured{}
		pg.SetGroupVersionKind(zalandoGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pgName, Namespace: ns.Name}, pg)).To(Succeed())

		memReq, _, _ := unstructured.NestedString(pg.Object, "spec", "resources", "requests", "memory")
		Expect(memReq).To(Equal("8Gi"))

		memLimit, _, _ := unstructured.NestedString(pg.Object, "spec", "resources", "limits", "memory")
		Expect(memLimit).To(Equal("8Gi")) // overcommit=1

		cpuReq, _, _ := unstructured.NestedString(pg.Object, "spec", "resources", "requests", "cpu")
		Expect(cpuReq).To(Equal("800m")) // 8Gi → 800m at 10:1 ratio

		// No maintenance history — this is bootstrap, not maintenance.
		updated := getPolicy(ctx, policy)
		Expect(updated.Status.MaintenanceHistory).To(BeEmpty())
		Expect(updated.Status.CurrentMemory).NotTo(BeNil())
		Expect(updated.Status.CurrentMemory.String()).To(Equal("8Gi"))
	})

	// ── Scenario 10: InitialMemory NOT applied when Zalando CR already has resources ─

	It("does not apply initialMemory when the Zalando CR already has resources", func() {
		vpaName := uniqueName("vpa")
		pgName := uniqueName("pg")
		makeVPA(ctx, ns.Name, vpaName, "20Gi", 0)
		makeZalandoCluster(ctx, ns.Name, pgName, "4Gi", "Running")

		initialMem := resource.MustParse("8Gi")
		policy := makePolicy(ctx, ns.Name, uniqueName("policy"), vpaName, pgName, cronNeverOpen)
		policy.Spec.InitialMemory = &initialMem
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())

		result, err := reconcilePolicy(ctx, policy)

		Expect(err).NotTo(HaveOccurred())
		// Should proceed to window check and requeue (window is closed).
		Expect(result.RequeueAfter).To(BeNumerically(">", 1*time.Minute))

		// Verify the Zalando CR was NOT changed.
		pg := &unstructured.Unstructured{}
		pg.SetGroupVersionKind(zalandoGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pgName, Namespace: ns.Name}, pg)).To(Succeed())

		memReq, _, _ := unstructured.NestedString(pg.Object, "spec", "resources", "requests", "memory")
		Expect(memReq).To(Equal("4Gi"))
	})

	// ── Scenario 11: InitialMemory not set – existing behavior unchanged ─────

	It("follows normal flow when initialMemory is not set and Zalando CR has no resources", func() {
		vpaName := uniqueName("vpa")
		pgName := uniqueName("pg")
		makeVPA(ctx, ns.Name, vpaName, "20Gi", 0)
		makeZalandoClusterNoResources(ctx, ns.Name, pgName, "Running")

		// No initialMemory set — should proceed to normal maintenance flow.
		policy := makePolicy(ctx, ns.Name, uniqueName("policy"), vpaName, pgName, cronAlwaysOpen)

		result, err := reconcilePolicy(ctx, policy)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))

		// Normal maintenance should have completed (currentMemory is nil → change gates pass → maintenance starts).
		updated := getPolicy(ctx, policy)
		Expect(updated.Status.MaintenanceHistory).To(HaveLen(1))
		Expect(updated.Status.MaintenanceHistory[0].Status).To(Equal(policyv1alpha1.MaintenanceStatusCompleted))
	})

	// ── Scenario 12: InitialMemory with overcommit > 1 ──────────────────────

	It("applies initialMemory with overcommit factor for memory limits", func() {
		vpaName := uniqueName("vpa")
		pgName := uniqueName("pg")
		makeVPA(ctx, ns.Name, vpaName, "20Gi", 0)
		makeZalandoClusterNoResources(ctx, ns.Name, pgName, "Running")

		initialMem := resource.MustParse("10Gi")
		policy := makePolicy(ctx, ns.Name, uniqueName("policy"), vpaName, pgName, cronNeverOpen)
		policy.Spec.InitialMemory = &initialMem
		policy.Spec.Overcommit = 1.5
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())

		result, err := reconcilePolicy(ctx, policy)

		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(1 * time.Minute))

		// Verify the Zalando CR was patched with overcommitted limits.
		pg := &unstructured.Unstructured{}
		pg.SetGroupVersionKind(zalandoGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pgName, Namespace: ns.Name}, pg)).To(Succeed())

		memReq, _, _ := unstructured.NestedString(pg.Object, "spec", "resources", "requests", "memory")
		Expect(memReq).To(Equal("10Gi"))

		memLimit, _, _ := unstructured.NestedString(pg.Object, "spec", "resources", "limits", "memory")
		Expect(memLimit).To(Equal("15Gi")) // 10Gi * 1.5 = 15GiB

		cpuReq, _, _ := unstructured.NestedString(pg.Object, "spec", "resources", "requests", "cpu")
		Expect(cpuReq).To(Equal("1")) // 10Gi → 1000m = 1 CPU
	})
})
