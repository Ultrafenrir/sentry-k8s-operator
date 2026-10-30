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

package v1alpha1

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

type sampleKustomization struct {
	Resources []string `yaml:"resources"`
}

var _ = Describe("Published samples", func() {
	It("passes the real CRD schema and admission webhooks", func() {
		sampleDir := filepath.Join("..", "..", "..", "config", "samples")
		kustomizationBytes, err := os.ReadFile(filepath.Join(sampleDir, "kustomization.yaml"))
		Expect(err).NotTo(HaveOccurred())

		var kustomization sampleKustomization
		Expect(yaml.Unmarshal(kustomizationBytes, &kustomization)).To(Succeed())
		Expect(kustomization.Resources).NotTo(BeEmpty())

		entries, err := os.ReadDir(sampleDir)
		Expect(err).NotTo(HaveOccurred())
		manifestNames := make([]string, 0, len(entries))
		for _, entry := range entries {
			if entry.IsDir() || entry.Name() == "kustomization.yaml" || filepath.Ext(entry.Name()) != ".yaml" {
				continue
			}
			manifestNames = append(manifestNames, entry.Name())
		}
		sort.Strings(manifestNames)
		sort.Strings(kustomization.Resources)
		Expect(kustomization.Resources).To(Equal(manifestNames), "every published YAML sample must be rendered by config/samples/kustomization.yaml")

		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "checkout-prod"}}
		err = k8sClient.Create(ctx, namespace)
		Expect(err == nil || apierrors.IsAlreadyExists(err)).To(BeTrue(), "create sample namespace: %v", err)

		for _, name := range kustomization.Resources {
			By("server-side validating " + name)
			manifest, readErr := os.ReadFile(filepath.Join(sampleDir, name))
			Expect(readErr).NotTo(HaveOccurred())

			decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
			objectCount := 0
			for {
				object := &unstructured.Unstructured{}
				decodeErr := decoder.Decode(object)
				if decodeErr == io.EOF {
					break
				}
				Expect(decodeErr).NotTo(HaveOccurred())
				if len(object.Object) == 0 {
					continue
				}

				objectCount++
				Expect(k8sClient.Create(ctx, object, client.DryRunAll)).To(Succeed())
				if object.GetKind() == "SentryProject" {
					onDelete, found, nestedErr := unstructured.NestedString(object.Object, "spec", "lifecycle", "onDelete")
					Expect(nestedErr).NotTo(HaveOccurred())
					Expect(found).To(BeTrue(), "the live mutating webhook must default spec.lifecycle.onDelete")
					Expect(onDelete).To(Equal("Retain"))
				}
			}
			Expect(objectCount).To(Equal(1), "%s must contain exactly one API object", name)
		}
	})
})
