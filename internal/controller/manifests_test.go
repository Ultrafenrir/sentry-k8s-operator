package controller

import (
	"errors"
	"io"
	"os"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestDeploymentManifestContracts(t *testing.T) {
	objects := readManifestObjects(t, "../../config/manager/manager.yaml")
	deploymentObject := findManifestObject(t, objects, "Deployment")
	deployment := &appsv1.Deployment{}
	convertManifestObject(t, deploymentObject, deployment)

	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 2 {
		t.Fatalf("controller replicas = %v, want 2", deployment.Spec.Replicas)
	}
	if deployment.Spec.MinReadySeconds != 5 {
		t.Fatalf("controller minReadySeconds = %d, want 5", deployment.Spec.MinReadySeconds)
	}
	rollingUpdate := deployment.Spec.Strategy.RollingUpdate
	if deployment.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType || rollingUpdate == nil ||
		rollingUpdate.MaxUnavailable == nil || rollingUpdate.MaxUnavailable.IntVal != 0 ||
		rollingUpdate.MaxSurge == nil || rollingUpdate.MaxSurge.IntVal != 1 {
		t.Fatalf("controller rolling update = %#v, want maxUnavailable=0 and maxSurge=1", deployment.Spec.Strategy)
	}
	podSpec := deployment.Spec.Template.Spec
	container := podSpec.Containers[0]
	assertContains(t, container.Args, "--leader-elect")
	if container.ReadinessProbe.HTTPGet.Path != "/readyz" {
		t.Fatalf("readiness path = %q, want /readyz", container.ReadinessProbe.HTTPGet.Path)
	}
	if container.LivenessProbe.HTTPGet.Path != "/healthz" {
		t.Fatalf("liveness path = %q, want /healthz", container.LivenessProbe.HTTPGet.Path)
	}
	if podSpec.SecurityContext.RunAsNonRoot == nil || !*podSpec.SecurityContext.RunAsNonRoot {
		t.Fatal("pod securityContext.runAsNonRoot must be true")
	}
	if container.SecurityContext.ReadOnlyRootFilesystem == nil || !*container.SecurityContext.ReadOnlyRootFilesystem {
		t.Fatal("container securityContext.readOnlyRootFilesystem must be true")
	}
	if podSpec.Affinity == nil || podSpec.Affinity.PodAntiAffinity == nil ||
		len(podSpec.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Fatal("controller must have one preferred pod anti-affinity rule")
	}

	pdb := &policyv1.PodDisruptionBudget{}
	convertManifestObject(t, readManifestObjects(t, "../../config/manager/pdb.yaml")[0], pdb)
	if pdb.Spec.MinAvailable == nil || pdb.Spec.MinAvailable.IntVal != 1 {
		t.Fatalf("PDB minAvailable = %v, want 1", pdb.Spec.MinAvailable)
	}
}

func readManifestObjects(t *testing.T, path string) []unstructured.Unstructured {
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

func findManifestObject(t *testing.T, objects []unstructured.Unstructured, kind string) unstructured.Unstructured {
	t.Helper()
	for _, object := range objects {
		if object.GetKind() == kind {
			return object
		}
	}
	t.Fatalf("manifest does not contain kind %s", kind)
	return unstructured.Unstructured{}
}

func convertManifestObject(t *testing.T, source unstructured.Unstructured, destination any) {
	t.Helper()
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(source.Object, destination); err != nil {
		t.Fatal(err)
	}
}

func assertContains(t *testing.T, values []string, expected string) {
	t.Helper()
	for _, value := range values {
		if value == expected {
			return
		}
	}
	t.Fatalf("%q is missing from %v", expected, values)
}
