package envtestprobe

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	admissionapi "k8s.io/api/admission/v1"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	"sigs.k8s.io/yaml"
)

const (
	probeNamespace = "sentry-operator-t00-probe"
	probeCRD       = "sentryprojectprobes.t00.sentry-operator.dev"
)

var probeGVR = schema.GroupVersionResource{
	Group: "t00.sentry-operator.dev", Version: "v1alpha1", Resource: "sentryprojectprobes",
}

func TestT00CRDContract(t *testing.T) {
	t.Parallel()

	repoRoot := os.Getenv("REPO_ROOT")
	if repoRoot == "" {
		t.Fatal("REPO_ROOT is required")
	}
	fixtureDir := filepath.Join(repoRoot, "testdata", "kubernetes", "t00-probes")
	testEnv := &envtest.Environment{
		BinaryAssetsDirectory:    os.Getenv("KUBEBUILDER_ASSETS"),
		CRDDirectoryPaths:        []string{filepath.Join(fixtureDir, "crds")},
		ErrorIfCRDPathMissing:    true,
		WebhookInstallOptions:    probeWebhookInstallOptions(),
		ControlPlaneStartTimeout: 60 * time.Second,
		ControlPlaneStopTimeout:  60 * time.Second,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})

	webhookOptions := testEnv.WebhookInstallOptions
	webhookServer := webhook.NewServer(webhook.Options{
		Host:    webhookOptions.LocalServingHost,
		Port:    webhookOptions.LocalServingPort,
		CertDir: webhookOptions.LocalServingCertDir,
	})
	webhookServer.Register("/mutate-t00-sentryprojectprobe", &webhook.Admission{
		Handler: defaultSecretHandler{},
	})
	webhookServer.Register("/validate-t00-sentryprojectprobe", &webhook.Admission{
		Handler: immutableProjectHandler{},
	})
	webhookContext, stopWebhooks := context.WithCancel(context.Background())
	t.Cleanup(stopWebhooks)
	go func() {
		_ = webhookServer.Start(webhookContext)
	}()
	waitForWebhookServer(t, webhookOptions.LocalServingHost, webhookOptions.LocalServingPort)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	coreClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("create core client: %v", err)
	}
	if _, err := coreClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: probeNamespace},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	dynamicClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("create dynamic client: %v", err)
	}
	probes := dynamicClient.Resource(probeGVR).Namespace(probeNamespace)

	managed := loadFixture(t, filepath.Join(fixtureDir, "valid-managed.yaml"))
	created, err := probes.Create(ctx, managed, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create valid managed probe: %v", err)
	}
	paused, found, err := unstructured.NestedBool(created.Object, "spec", "paused")
	if err != nil || !found || paused {
		t.Fatalf("expected default spec.paused=false, found=%v value=%v err=%v", found, paused, err)
	}
	onDelete, found, err := unstructured.NestedString(created.Object, "spec", "lifecycle", "onDelete")
	if err != nil || !found || onDelete != "Retain" {
		t.Fatalf("expected default lifecycle.onDelete=Retain, found=%v value=%q err=%v", found, onDelete, err)
	}
	secretName, found, err := unstructured.NestedString(created.Object, "spec", "dsn", "secret")
	if err != nil || !found || secretName != "managed-sentry" {
		t.Fatalf("expected webhook default dsn.secret=managed-sentry, found=%v value=%q err=%v", found, secretName, err)
	}

	statusObject := created.DeepCopy()
	statusObject.Object["status"] = map[string]any{
		"observedGeneration": created.GetGeneration(),
		"registrationCommit": "probe",
	}
	updated, err := probes.UpdateStatus(ctx, statusObject, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("update status subresource: %v", err)
	}
	if updated.GetGeneration() != created.GetGeneration() {
		t.Fatalf("status update changed generation: before=%d after=%d", created.GetGeneration(), updated.GetGeneration())
	}
	commit, found, err := unstructured.NestedString(updated.Object, "status", "registrationCommit")
	if err != nil || !found || commit != "probe" {
		t.Fatalf("status round-trip failed, found=%v value=%q err=%v", found, commit, err)
	}

	fencedDestinationChange := updated.DeepCopy()
	if err := unstructured.SetNestedMap(
		fencedDestinationChange.Object,
		map[string]any{"telegram": map[string]any{"target": "payments-alerts"}},
		"spec", "destinations",
	); err != nil {
		t.Fatalf("prepare fenced destination update: %v", err)
	}
	if _, err := probes.Update(ctx, fencedDestinationChange, metav1.UpdateOptions{}); err == nil ||
		!apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "PolicyCommitInProgress") {
		t.Fatalf("expected active registration fence rejection, got %v", err)
	}

	clearedFence := updated.DeepCopy()
	if err := unstructured.SetNestedField(
		clearedFence.Object, "", "status", "registrationCommit",
	); err != nil {
		t.Fatalf("prepare registration fence clear: %v", err)
	}
	clearedFence, err = probes.UpdateStatus(ctx, clearedFence, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("clear registration fence: %v", err)
	}

	allowedDestinationChange := clearedFence.DeepCopy()
	if err := unstructured.SetNestedMap(
		allowedDestinationChange.Object,
		map[string]any{"telegram": map[string]any{"target": "payments-alerts"}},
		"spec", "destinations",
	); err != nil {
		t.Fatalf("prepare destination update after fence clear: %v", err)
	}
	allowedDestinationChange, err = probes.Update(ctx, allowedDestinationChange, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("update destination after registration fence clear: %v", err)
	}

	staleUpdate := allowedDestinationChange.DeepCopy()
	freshUpdate := allowedDestinationChange.DeepCopy()
	if err := unstructured.SetNestedField(freshUpdate.Object, true, "spec", "paused"); err != nil {
		t.Fatalf("prepare fresh update: %v", err)
	}
	if _, err := probes.Update(ctx, freshUpdate, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("apply fresh update: %v", err)
	}
	if err := unstructured.SetNestedField(staleUpdate.Object, false, "spec", "paused"); err != nil {
		t.Fatalf("prepare stale update: %v", err)
	}
	if _, err := probes.Update(ctx, staleUpdate, metav1.UpdateOptions{}); !apierrors.IsConflict(err) {
		t.Fatalf("expected stale resourceVersion conflict, got %v", err)
	}

	changedProject, err := probes.Get(ctx, created.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get probe before immutable update: %v", err)
	}
	if err := unstructured.SetNestedField(
		changedProject.Object, "other-team", "spec", "project", "create", "team",
	); err != nil {
		t.Fatalf("prepare immutable project update: %v", err)
	}
	if _, err := probes.Update(ctx, changedProject, metav1.UpdateOptions{}); err == nil ||
		!apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "spec.project is immutable") {
		t.Fatalf("expected validating webhook immutable rejection, got %v", err)
	}

	validExternal := loadFixture(t, filepath.Join(fixtureDir, "valid-external.yaml"))
	if _, err := probes.Create(ctx, validExternal, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create valid external probe: %v", err)
	}

	negative := []struct {
		fixture string
		message string
	}{
		{"invalid-create-and-use.yaml", "exactly one of project.create or project.use is required"},
		{"invalid-neither-create-nor-use.yaml", "exactly one of project.create or project.use is required"},
		{"invalid-use-and-settings.yaml", "project.use forbids settings, destinations and alerts"},
		{"invalid-use-and-alerts.yaml", "project.use forbids settings, destinations and alerts"},
		{"invalid-use-and-delete.yaml", "project.use forbids lifecycle.onDelete DeleteProject"},
	}
	for _, test := range negative {
		t.Run(test.fixture, func(t *testing.T) {
			object := loadFixture(t, filepath.Join(fixtureDir, test.fixture))
			_, err := probes.Create(ctx, object, metav1.CreateOptions{})
			if err == nil {
				t.Fatalf("expected API server rejection")
			}
			if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("unexpected rejection: %v", err)
			}
		})
	}

	extensionsClient, err := apiextensionsclient.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("create apiextensions client: %v", err)
	}
	crd, err := extensionsClient.ApiextensionsV1().CustomResourceDefinitions().Get(
		ctx, probeCRD, metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get CRD: %v", err)
	}
	if crd.Spec.Conversion == nil || crd.Spec.Conversion.Strategy != apiextensionsv1.NoneConverter {
		t.Fatalf("expected conversion strategy None, got %#v", crd.Spec.Conversion)
	}
}

type defaultSecretHandler struct{}

func (defaultSecretHandler) Handle(_ context.Context, request admission.Request) admission.Response {
	object := &unstructured.Unstructured{}
	if err := json.Unmarshal(request.Object.Raw, &object.Object); err != nil {
		return admission.Errored(400, err)
	}
	if _, found, err := unstructured.NestedString(object.Object, "spec", "dsn", "secret"); err != nil {
		return admission.Errored(400, err)
	} else if found {
		return admission.Allowed("dsn secret already set")
	}
	if err := unstructured.SetNestedField(
		object.Object, object.GetName()+"-sentry", "spec", "dsn", "secret",
	); err != nil {
		return admission.Errored(500, err)
	}
	mutated, err := json.Marshal(object.Object)
	if err != nil {
		return admission.Errored(500, err)
	}
	return admission.PatchResponseFromRaw(request.Object.Raw, mutated)
}

type immutableProjectHandler struct{}

func (immutableProjectHandler) Handle(_ context.Context, request admission.Request) admission.Response {
	if request.Operation != admissionapi.Update {
		return admission.Allowed("create")
	}
	oldObject := &unstructured.Unstructured{}
	newObject := &unstructured.Unstructured{}
	if err := json.Unmarshal(request.OldObject.Raw, &oldObject.Object); err != nil {
		return admission.Errored(400, err)
	}
	if err := json.Unmarshal(request.Object.Raw, &newObject.Object); err != nil {
		return admission.Errored(400, err)
	}
	oldProject, _, err := unstructured.NestedMap(oldObject.Object, "spec", "project")
	if err != nil {
		return admission.Errored(400, err)
	}
	newProject, _, err := unstructured.NestedMap(newObject.Object, "spec", "project")
	if err != nil {
		return admission.Errored(400, err)
	}
	if !reflect.DeepEqual(oldProject, newProject) {
		return admission.Denied("spec.project is immutable")
	}
	registrationCommit, _, err := unstructured.NestedString(
		oldObject.Object, "status", "registrationCommit",
	)
	if err != nil {
		return admission.Errored(400, err)
	}
	oldDestinations, _, err := unstructured.NestedMap(oldObject.Object, "spec", "destinations")
	if err != nil {
		return admission.Errored(400, err)
	}
	newDestinations, _, err := unstructured.NestedMap(newObject.Object, "spec", "destinations")
	if err != nil {
		return admission.Errored(400, err)
	}
	if registrationCommit != "" && !reflect.DeepEqual(oldDestinations, newDestinations) {
		return admission.Denied("PolicyCommitInProgress: destinations are fenced")
	}
	return admission.Allowed("project unchanged")
}

func probeWebhookInstallOptions() envtest.WebhookInstallOptions {
	failurePolicy := admissionv1.Fail
	sideEffects := admissionv1.SideEffectClassNone
	scope := admissionv1.NamespacedScope
	mutatingPath := "/mutate-t00-sentryprojectprobe"
	validatingPath := "/validate-t00-sentryprojectprobe"
	rules := []admissionv1.RuleWithOperations{{
		Operations: []admissionv1.OperationType{admissionv1.Create, admissionv1.Update},
		Rule: admissionv1.Rule{
			APIGroups:   []string{"t00.sentry-operator.dev"},
			APIVersions: []string{"v1alpha1"},
			Resources:   []string{"sentryprojectprobes"},
			Scope:       &scope,
		},
	}}
	return envtest.WebhookInstallOptions{
		MutatingWebhooks: []*admissionv1.MutatingWebhookConfiguration{{
			ObjectMeta: metav1.ObjectMeta{Name: "t00-sentryprojectprobe-defaulting"},
			Webhooks: []admissionv1.MutatingWebhook{{
				Name:                    "default.sentryprojectprobe.t00.sentry-operator.dev",
				Rules:                   rules,
				FailurePolicy:           &failurePolicy,
				SideEffects:             &sideEffects,
				AdmissionReviewVersions: []string{"v1"},
				ClientConfig: admissionv1.WebhookClientConfig{Service: &admissionv1.ServiceReference{
					Name: "probe", Namespace: "default", Path: &mutatingPath,
				}},
			}},
		}},
		ValidatingWebhooks: []*admissionv1.ValidatingWebhookConfiguration{{
			ObjectMeta: metav1.ObjectMeta{Name: "t00-sentryprojectprobe-validation"},
			Webhooks: []admissionv1.ValidatingWebhook{{
				Name:                    "validate.sentryprojectprobe.t00.sentry-operator.dev",
				Rules:                   rules,
				FailurePolicy:           &failurePolicy,
				SideEffects:             &sideEffects,
				AdmissionReviewVersions: []string{"v1"},
				ClientConfig: admissionv1.WebhookClientConfig{Service: &admissionv1.ServiceReference{
					Name: "probe", Namespace: "default", Path: &validatingPath,
				}},
			}},
		}},
	}
}

func waitForWebhookServer(t *testing.T, host string, port int) {
	t.Helper()
	address := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("webhook server did not listen on %s", address)
}

func loadFixture(t *testing.T, path string) *unstructured.Unstructured {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	jsonData, err := yaml.YAMLToJSON(data)
	if err != nil {
		t.Fatalf("convert fixture %s to JSON: %v", path, err)
	}
	object := &unstructured.Unstructured{}
	if err := json.Unmarshal(jsonData, &object.Object); err != nil {
		t.Fatalf("decode fixture %s: %v", path, err)
	}
	object.SetNamespace(probeNamespace)
	return object
}
