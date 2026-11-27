package controller

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestClusterSecretRBACIsRestrictedToFixedNamespaceSecrets(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile("../../charts/sentry-kubernetes-operator/templates/rbac.yaml")
	if err != nil {
		t.Fatal(err)
	}
	template := string(content)
	for _, required := range []string{
		`resourceNames: ["sentry-dsn"]`,
		`verbs: ["get", "patch", "update"]`,
		`resources: ["secrets"]
    verbs: ["create"]`,
		`resourceNames: ["sentry-project-inputs"]`,
		`verbs: ["get"]`,
	} {
		if !strings.Contains(template, required) {
			t.Fatalf("RBAC template is missing %q", required)
		}
	}
	clusterRole := strings.Split(template, "kind: ClusterRoleBinding")[0]
	if strings.Contains(clusterRole, `verbs: ["create", "delete"`) ||
		strings.Contains(clusterRole, `resources: ["secrets"]
    verbs: ["get"`) ||
		strings.Contains(clusterRole, `resources: ["secrets"]
    verbs: ["list"`) {
		t.Fatal("ClusterRole grants unrestricted Secret mutation")
	}
}

func TestCRDsDoNotRequireCELValidationSupport(t *testing.T) {
	t.Parallel()

	paths := []string{
		"../../charts/sentry-kubernetes-operator/crds/sentryconfiguration.io_sentryalertpolicies.yaml",
		"../../charts/sentry-kubernetes-operator/crds/sentryconfiguration.io_sentryconnections.yaml",
		"../../charts/sentry-kubernetes-operator/crds/sentryconfiguration.io_sentrycronmonitors.yaml",
		"../../charts/sentry-kubernetes-operator/crds/sentryconfiguration.io_sentryprojects.yaml",
		"../../charts/sentry-kubernetes-operator/crds/sentryconfiguration.io_sentryuptimemonitors.yaml",
	}
	for _, path := range paths {
		objects := readManifestObjects(t, path)
		if len(objects) != 1 {
			t.Fatalf("%s contains %d objects, want 1", path, len(objects))
		}
		if containsMapKey(objects[0].Object, "x-kubernetes-validations") {
			t.Fatalf("%s requires CRD CEL validation support", path)
		}
	}
}

func TestHelmChartContainsEveryControllerCRD(t *testing.T) {
	t.Parallel()
	paths, err := filepath.Glob("../../charts/sentry-kubernetes-operator/crds/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 5 {
		t.Fatalf("chart contains %d CRD manifests, want exactly 5: %v", len(paths), paths)
	}
	want := map[string]bool{
		"SentryAlertPolicy":   false,
		"SentryConnection":    false,
		"SentryCronMonitor":   false,
		"SentryProject":       false,
		"SentryUptimeMonitor": false,
	}
	for _, path := range paths {
		objects := readManifestObjects(t, path)
		if len(objects) != 1 {
			t.Fatalf("%s contains %d objects, want 1", path, len(objects))
		}
		kind, _, _ := unstructured.NestedString(objects[0].Object, "spec", "names", "kind")
		if _, found := want[kind]; !found {
			t.Fatalf("%s declares unexpected CRD kind %q", path, kind)
		}
		if want[kind] {
			t.Fatalf("CRD kind %q is declared more than once", kind)
		}
		want[kind] = true
	}
	for kind, found := range want {
		if !found {
			t.Errorf("chart is missing CRD kind %q", kind)
		}
	}
}

func containsMapKey(value any, key string) bool {
	switch typed := value.(type) {
	case map[string]any:
		for name, child := range typed {
			if name == key || containsMapKey(child, key) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsMapKey(child, key) {
				return true
			}
		}
	}
	return false
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
