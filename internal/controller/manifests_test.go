package controller

import (
	"errors"
	"io"
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestCRDsDoNotRequireCELValidationSupport(t *testing.T) {
	t.Parallel()

	paths := []string{
		"../../charts/sentry-kubernetes-operator/crds/sentryconfiguration.io_sentryalertpolicies.yaml",
		"../../charts/sentry-kubernetes-operator/crds/sentryconfiguration.io_sentryconnections.yaml",
		"../../charts/sentry-kubernetes-operator/crds/sentryconfiguration.io_sentryprojects.yaml",
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
