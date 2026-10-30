/*
Copyright 2026 Sentry Kubernetes Operator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package telegramdelivery

import (
	"errors"
	"io"
	"os"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// TestDeliveryManifestContracts parses manifests without REST mapping or API
// discovery. This keeps source verification independent of the current
// kube-context while still using the Kubernetes typed API as the contract.
func TestDeliveryManifestContracts(t *testing.T) {
	stateRoot := "../../config/state/"
	assertKustomizationResources(t, stateRoot+"kustomization.yaml", []string{
		"namespace.yaml", "routing-namespace.yaml", "queue-namespace.yaml", "audit-namespace.yaml", "hook-source-namespace.yaml", "rbac.yaml", "routing-rbac.yaml", "audit-rbac.yaml", "audit-admission.yaml", "hook-source-rbac.yaml",
	})
	for path, expectedName := range map[string]string{
		"namespace.yaml":             "sentry-kubernetes-operator-state",
		"routing-namespace.yaml":     "sentry-kubernetes-operator-routing",
		"queue-namespace.yaml":       "sentry-kubernetes-operator-delivery",
		"audit-namespace.yaml":       "sentry-kubernetes-operator-audit",
		"hook-source-namespace.yaml": "sentry-kubernetes-operator-hook-sources",
	} {
		namespace := &corev1.Namespace{}
		convertDeliveryManifest(t, readDeliveryManifestObjects(t, stateRoot+path)[0], namespace)
		if namespace.Name != expectedName {
			t.Errorf("namespace in %s = %q, want %q", path, namespace.Name, expectedName)
		}
	}

	routingManager := &rbacv1.Role{}
	convertDeliveryManifest(t, findDeliveryManifest(t, readDeliveryManifestObjects(t, stateRoot+"routing-rbac.yaml"), "Role", "sentry-kubernetes-operator-protected-routing-manager"), routingManager)
	assertSecretRule(t, routingManager.Rules, []string{"create", "delete", "get"})
	auditObjects := readDeliveryManifestObjects(t, stateRoot+"audit-rbac.yaml")
	auditManager := &rbacv1.Role{}
	convertDeliveryManifest(t, findDeliveryManifest(t, auditObjects, "Role", "sentry-kubernetes-operator-protected-audit-manager"), auditManager)
	assertSecretRule(t, auditManager.Rules, []string{"create", "delete", "get", "list"})
	auditReader := &rbacv1.Role{}
	convertDeliveryManifest(t, findDeliveryManifest(t, auditObjects, "Role", "sentry-kubernetes-operator-protected-audit-reader"), auditReader)
	assertSecretRule(t, auditReader.Rules, []string{"list"})
	hookSourceObjects := readDeliveryManifestObjects(t, stateRoot+"hook-source-rbac.yaml")
	hookSourceManager := &rbacv1.Role{}
	convertDeliveryManifest(t, findDeliveryManifest(t, hookSourceObjects, "Role", "sentry-kubernetes-operator-protected-hook-source-manager"), hookSourceManager)
	assertSecretRule(t, hookSourceManager.Rules, []string{"create", "delete", "get", "list", "patch", "update", "watch"})

	rbacObjects := readDeliveryManifestObjects(t, "../../config/registration/delivery-rbac.yaml")
	routingRole := &rbacv1.Role{}
	convertDeliveryManifest(t, findDeliveryManifest(t, rbacObjects, "Role", "telegram-delivery-worker-routing"), routingRole)
	assertSecretRule(t, routingRole.Rules, []string{"get"})
	queueRole := &rbacv1.Role{}
	convertDeliveryManifest(t, findDeliveryManifest(t, rbacObjects, "Role", "telegram-delivery-worker-queue"), queueRole)
	assertSecretRule(t, queueRole.Rules, []string{"delete", "get", "list", "update"})
	for _, role := range []*rbacv1.Role{routingRole, queueRole} {
		for _, rule := range role.Rules {
			if slices.Contains(rule.Verbs, "create") {
				t.Errorf("worker role %s unexpectedly grants create", role.Name)
			}
		}
	}
	auditWriterObjects := readDeliveryManifestObjects(t, "../../config/registration/audit-rbac.yaml")
	auditWriters := &rbacv1.Role{}
	convertDeliveryManifest(t, findDeliveryManifest(t, auditWriterObjects, "Role", "mutation-audit-writers"), auditWriters)
	assertSecretRule(t, auditWriters.Rules, []string{"create"})
	producerRBACObjects := readDeliveryManifestObjects(t, "../../config/registration/hook-producer-rbac.yaml")
	producerSources := &rbacv1.Role{}
	convertDeliveryManifest(t, findDeliveryManifest(t, producerRBACObjects, "Role", "sentry-hook-producer-sources"), producerSources)
	assertSecretRule(t, producerSources.Rules, []string{"get"})
	producerQueue := &rbacv1.Role{}
	convertDeliveryManifest(t, findDeliveryManifest(t, producerRBACObjects, "Role", "sentry-hook-producer-queue"), producerQueue)
	assertSecretRule(t, producerQueue.Rules, []string{"create", "delete", "get", "list"})
	for _, rule := range producerQueue.Rules {
		if slices.Contains(rule.Verbs, "update") || slices.Contains(rule.Verbs, "patch") {
			t.Fatalf("producer queue role grants mutable queue verbs: %+v", rule.Verbs)
		}
	}

	producerObjects := readDeliveryManifestObjects(t, "../../config/registration/hook-producer.yaml")
	producerDeployment := &appsv1.Deployment{}
	convertDeliveryManifest(t, findDeliveryManifest(t, producerObjects, "Deployment", "sentry-hook-producer"), producerDeployment)
	if producerDeployment.Spec.Replicas == nil || *producerDeployment.Spec.Replicas != 2 || producerDeployment.Spec.Template.Spec.ServiceAccountName != "sentry-hook-producer" {
		t.Fatalf("producer HA/service account contract is invalid: %+v", producerDeployment.Spec)
	}
	producerContainer := producerDeployment.Spec.Template.Spec.Containers[0]
	if !slices.Equal(producerContainer.Command, []string{"/sentry-hook-producer"}) || producerContainer.ReadinessProbe.HTTPGet.Path != "/healthz" || producerContainer.ReadinessProbe.HTTPGet.Port.StrVal != "health" {
		t.Fatalf("producer command or isolated health probe is invalid: %+v", producerContainer)
	}

	receiverObjects := readDeliveryManifestObjects(t, "../../config/registration/hook-receiver.yaml")
	receiverAccount := &corev1.ServiceAccount{}
	convertDeliveryManifest(t, findDeliveryManifest(t, receiverObjects, "ServiceAccount", "sentry-hook-receiver"), receiverAccount)
	if receiverAccount.AutomountServiceAccountToken == nil || *receiverAccount.AutomountServiceAccountToken {
		t.Fatal("public Sentry receiver service account must be tokenless")
	}
	receiverDeployment := &appsv1.Deployment{}
	convertDeliveryManifest(t, findDeliveryManifest(t, receiverObjects, "Deployment", "sentry-hook-receiver"), receiverDeployment)
	receiverPod := receiverDeployment.Spec.Template.Spec
	if receiverDeployment.Spec.Replicas == nil || *receiverDeployment.Spec.Replicas != 2 || receiverPod.AutomountServiceAccountToken == nil || *receiverPod.AutomountServiceAccountToken || len(receiverPod.Containers) != 1 {
		t.Fatalf("receiver HA/tokenless contract is invalid: %+v", receiverDeployment.Spec)
	}
	if !slices.Equal(receiverPod.Containers[0].Command, []string{"/sentry-hook-receiver"}) {
		t.Fatalf("receiver command = %v", receiverPod.Containers[0].Command)
	}
	if receiverPod.Containers[0].ReadinessProbe == nil || receiverPod.Containers[0].ReadinessProbe.HTTPGet == nil || receiverPod.Containers[0].ReadinessProbe.HTTPGet.Path != "/readyz" ||
		receiverPod.Containers[0].LivenessProbe == nil || receiverPod.Containers[0].LivenessProbe.HTTPGet == nil || receiverPod.Containers[0].LivenessProbe.HTTPGet.Path != "/healthz" {
		t.Fatal("Sentry receiver must separate projected-config readiness from liveness")
	}

	workerObjects := readDeliveryManifestObjects(t, "../../config/registration/delivery-worker.yaml")
	deployment := &appsv1.Deployment{}
	convertDeliveryManifest(t, findDeliveryManifest(t, workerObjects, "Deployment", "telegram-delivery-worker"), deployment)
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 2 {
		t.Fatalf("worker replicas = %v, want 2", deployment.Spec.Replicas)
	}
	podSpec := deployment.Spec.Template.Spec
	if podSpec.ServiceAccountName != "telegram-delivery-worker" || len(podSpec.Containers) != 1 {
		t.Fatalf("worker pod contract = serviceAccount %q, containers %d", podSpec.ServiceAccountName, len(podSpec.Containers))
	}
	container := podSpec.Containers[0]
	if !slices.Equal(container.Command, []string{"/telegram-delivery-worker"}) || !slices.Contains(container.Args, "--metrics-address=:8080") {
		t.Errorf("worker command/args = %v %v", container.Command, container.Args)
	}
	if container.ReadinessProbe == nil || container.ReadinessProbe.HTTPGet == nil || container.ReadinessProbe.HTTPGet.Path != "/readyz" ||
		container.LivenessProbe == nil || container.LivenessProbe.HTTPGet == nil || container.LivenessProbe.HTTPGet.Path != "/healthz" {
		t.Fatal("worker must expose the expected readiness and liveness HTTP probes")
	}
	metricsPort := false
	for _, port := range container.Ports {
		metricsPort = metricsPort || (port.Name == "metrics" && port.ContainerPort == 8080)
	}
	if !metricsPort {
		t.Fatal("worker metrics container port 8080 is missing")
	}
	service := &corev1.Service{}
	convertDeliveryManifest(t, findDeliveryManifest(t, workerObjects, "Service", "telegram-delivery-worker-metrics"), service)
	if len(service.Spec.Ports) != 1 || service.Spec.Ports[0].Port != 8080 || service.Spec.Ports[0].TargetPort.StrVal != "metrics" {
		t.Errorf("metrics service ports = %+v", service.Spec.Ports)
	}
	pdb := &policyv1.PodDisruptionBudget{}
	convertDeliveryManifest(t, findDeliveryManifest(t, workerObjects, "PodDisruptionBudget", "telegram-delivery-worker"), pdb)
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntVal != 1 {
		t.Errorf("worker PDB minAvailable = %v, want 1", pdb.Spec.MinAvailable)
	}

	networkPolicy := &networkingv1.NetworkPolicy{}
	convertDeliveryManifest(t, readDeliveryManifestObjects(t, "../../config/registration/delivery-network-policy.yaml")[0], networkPolicy)
	if !slices.Contains(networkPolicy.Spec.PolicyTypes, networkingv1.PolicyTypeIngress) || !slices.Contains(networkPolicy.Spec.PolicyTypes, networkingv1.PolicyTypeEgress) {
		t.Fatalf("network policy types = %v, want Ingress and Egress", networkPolicy.Spec.PolicyTypes)
	}
	metricsIngress := false
	for _, ingress := range networkPolicy.Spec.Ingress {
		for _, source := range ingress.From {
			for _, port := range ingress.Ports {
				metricsIngress = metricsIngress || (source.NamespaceSelector != nil && source.NamespaceSelector.MatchLabels["metrics"] == "enabled" && port.Port != nil && port.Port.IntVal == 8080)
			}
		}
	}
	if !metricsIngress || !networkPolicyAllowsEgressPort(networkPolicy, 443) || !networkPolicyAllowsEgressPort(networkPolicy, 6443) || !networkPolicyAllowsEgressPort(networkPolicy, 53) {
		t.Fatalf("network policy lacks metrics ingress or required 443/6443/53 egress: %+v", networkPolicy.Spec)
	}
}

func readDeliveryManifestObjects(t *testing.T, path string) []unstructured.Unstructured {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := utilyaml.NewYAMLOrJSONDecoder(file, 4096)
	var objects []unstructured.Unstructured
	for {
		var object unstructured.Unstructured
		err = decoder.Decode(&object)
		if errors.Is(err, io.EOF) {
			return objects
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(object.Object) != 0 {
			objects = append(objects, object)
		}
	}
}

func findDeliveryManifest(t *testing.T, objects []unstructured.Unstructured, kind, name string) unstructured.Unstructured {
	t.Helper()
	for _, object := range objects {
		if object.GetKind() == kind && object.GetName() == name {
			return object
		}
	}
	t.Fatalf("manifest does not contain %s/%s", kind, name)
	return unstructured.Unstructured{}
}

func convertDeliveryManifest(t *testing.T, source unstructured.Unstructured, destination any) {
	t.Helper()
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(source.Object, destination); err != nil {
		t.Fatal(err)
	}
}

func assertSecretRule(t *testing.T, rules []rbacv1.PolicyRule, expectedVerbs []string) {
	t.Helper()
	for _, rule := range rules {
		verbs := slices.Clone(rule.Verbs)
		slices.Sort(verbs)
		if slices.Equal(rule.APIGroups, []string{""}) && slices.Equal(rule.Resources, []string{"secrets"}) && slices.Equal(verbs, expectedVerbs) {
			return
		}
	}
	t.Fatalf("secret rule with verbs %v is missing from %+v", expectedVerbs, rules)
}

func networkPolicyAllowsEgressPort(policy *networkingv1.NetworkPolicy, expected int32) bool {
	for _, egress := range policy.Spec.Egress {
		for _, port := range egress.Ports {
			if port.Port != nil && port.Port.IntVal == expected {
				return true
			}
		}
	}
	return false
}

func assertKustomizationResources(t *testing.T, path string, expected []string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var kustomization struct {
		Resources []string `json:"resources"`
	}
	if err := yaml.Unmarshal(contents, &kustomization); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(kustomization.Resources, expected) {
		t.Fatalf("kustomization resources = %v, want %v", kustomization.Resources, expected)
	}
}
