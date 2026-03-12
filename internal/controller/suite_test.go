// Package controller_test contains envtest integration tests for the
// zalando-vertical-autoscaler controller.
package controller_test

import (
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	vpav1 "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	policyv1alpha1 "github.com/pricefx/zalando-vertical-autoscaler/api/v1alpha1"
	"github.com/pricefx/zalando-vertical-autoscaler/internal/controller"
)

// TestControllers is the entry point for the Ginkgo integration test suite.
func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controller Integration Suite")
}

var (
	testEnv    *envtest.Environment
	k8sClient  client.Client
	reconciler *controller.PostgresMemoryPolicyReconciler
)

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	scheme := k8sruntime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(vpav1.AddToScheme(scheme))
	utilruntime.Must(policyv1alpha1.AddToScheme(scheme))

	// CRD paths are relative to this file's directory (internal/controller/).
	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd"),
			filepath.Join("..", "..", "config", "testdata"),
		},
		ErrorIfCRDPathMissing: true,
		Scheme:               scheme,
	}

	cfg, err := testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	// FakeRecorder satisfies record.EventRecorder; buffer is large enough
	// that the tests never block on unread events.
	fakeRecorder := record.NewFakeRecorder(200)
	reconciler = controller.NewPostgresMemoryPolicyReconciler(k8sClient, scheme, fakeRecorder)
})

var _ = AfterSuite(func() {
	Expect(testEnv.Stop()).To(Succeed())
})
