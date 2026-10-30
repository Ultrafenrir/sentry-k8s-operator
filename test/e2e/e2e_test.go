//go:build e2e
// +build e2e

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

package e2e

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/artemantipov/sentry-kubernetes-operator/test/utils"
)

// namespace where the project is deployed in
const namespace = "sentry-kubernetes-operator-system"

// serviceAccountName created for the project
const serviceAccountName = "sentry-kubernetes-operator-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "sentry-kubernetes-operator-controller-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "sentry-kubernetes-operator-metrics-binding"

var _ = Describe("Manager", Ordered, func() {
	var controllerPodName string

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted", "metrics=enabled")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		By("cleaning up the curl pod for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace)
		_, _ = utils.Run(cmd)

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				By("getting the name of the controller-manager pod")
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(2), "expected both HA controller pods to be running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				By("validating the pod's status")
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			cmd := exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=sentry-kubernetes-operator-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("ensuring the controller pod is ready")
			verifyControllerPodReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", controllerPodName, "-n", namespace,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"), "Controller pod not ready")
			}
			Eventually(verifyControllerPodReady, 3*time.Minute, time.Second).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("Serving metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted, 3*time.Minute, time.Second).Should(Succeed())

			By("waiting for the webhook service endpoints to be ready")
			verifyWebhookEndpointsReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "endpointslices.discovery.k8s.io", "-n", namespace,
					"-l", "kubernetes.io/service-name=sentry-kubernetes-operator-webhook-service",
					"-o", "jsonpath={range .items[*]}{range .endpoints[*]}{.addresses[*]}{end}{end}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Webhook endpoints should exist")
				g.Expect(output).ShouldNot(BeEmpty(), "Webhook endpoints not yet ready")
			}
			Eventually(verifyWebhookEndpointsReady, 3*time.Minute, time.Second).Should(Succeed())

			By("verifying the mutating webhook server is ready")
			verifyMutatingWebhookReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "mutatingwebhookconfigurations.admissionregistration.k8s.io",
					"sentry-kubernetes-operator-mutating-webhook-configuration",
					"-o", "jsonpath={.webhooks[0].clientConfig.caBundle}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "MutatingWebhookConfiguration should exist")
				g.Expect(output).ShouldNot(BeEmpty(), "Mutating webhook CA bundle not yet injected")
			}
			Eventually(verifyMutatingWebhookReady, 3*time.Minute, time.Second).Should(Succeed())

			By("verifying the validating webhook server is ready")
			verifyValidatingWebhookReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "validatingwebhookconfigurations.admissionregistration.k8s.io",
					"sentry-kubernetes-operator-validating-webhook-configuration",
					"-o", "jsonpath={.webhooks[0].clientConfig.caBundle}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "ValidatingWebhookConfiguration should exist")
				g.Expect(output).ShouldNot(BeEmpty(), "Validating webhook CA bundle not yet injected")
			}
			Eventually(verifyValidatingWebhookReady, 3*time.Minute, time.Second).Should(Succeed())

			By("waiting additional time for webhook server to stabilize")
			time.Sleep(5 * time.Second)

			// +kubebuilder:scaffold:e2e-metrics-webhooks-readiness

			By("creating the curl-metrics pod to access the metrics endpoint")
			err = createMetricsProbe("curl-metrics")
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			verifyMetricsAvailable := func(g Gomega) {
				metricsOutput, err := getMetricsOutput()
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
				g.Expect(metricsOutput).NotTo(BeEmpty())
				g.Expect(metricsOutput).To(ContainSubstring("METRICS_OK"))
			}
			Eventually(verifyMetricsAvailable, 2*time.Minute).Should(Succeed())
		})

		It("should provisioned cert-manager", func() {
			By("validating that cert-manager has the certificate Secret")
			verifyCertManager := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "secrets", "webhook-server-cert", "-n", namespace)
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}
			Eventually(verifyCertManager).Should(Succeed())
		})

		It("should reload the metrics certificate without restarting controller pods", func() {
			beforePods, err := controllerPodIdentities()
			Expect(err).NotTo(HaveOccurred())
			Expect(beforePods).To(HaveLen(2))

			cmd := exec.Command("kubectl", "get", "secret", "metrics-server-cert", "-n", namespace,
				"-o", "jsonpath={.metadata.uid}")
			oldSecretUID, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(oldSecretUID).NotTo(BeEmpty())
			rotationStarted := time.Now().UTC().Add(-time.Second).Format(time.RFC3339)

			By("deleting the metrics certificate Secret so cert-manager reissues it")
			_, err = utils.Run(exec.Command("kubectl", "delete", "secret", "metrics-server-cert", "-n", namespace))
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "secret", "metrics-server-cert", "-n", namespace,
					"-o", "jsonpath={.metadata.uid}")
				newSecretUID, getErr := utils.Run(cmd)
				g.Expect(getErr).NotTo(HaveOccurred())
				g.Expect(newSecretUID).NotTo(BeEmpty())
				g.Expect(newSecretUID).NotTo(Equal(oldSecretUID))
			}, 2*time.Minute, time.Second).Should(Succeed())

			By("waiting for both controller certificate watchers to load the projected Secret")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", "-n", namespace,
					"-l", "control-plane=controller-manager", "--prefix", "--since-time", rotationStarted)
				logs, getErr := utils.Run(cmd)
				g.Expect(getErr).NotTo(HaveOccurred())
				updates := 0
				for _, line := range strings.Split(logs, "\n") {
					if strings.Contains(line, "Updated current TLS certificate") &&
						strings.Contains(line, "/tmp/k8s-metrics-server/metrics-certs/tls.crt") {
						updates++
					}
				}
				g.Expect(updates).To(BeNumerically(">=", 2), "expected rotated metrics certificate on both replicas")
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			const rotatedProbe = "curl-metrics-rotated"
			DeferCleanup(func() {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", rotatedProbe, "-n", namespace, "--ignore-not-found"))
			})
			By("scraping metrics with the reissued CA")
			Expect(createMetricsProbe(rotatedProbe)).To(Succeed())
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", rotatedProbe, "-n", namespace,
					"-o", "jsonpath={.status.phase}")
				phase, getErr := utils.Run(cmd)
				g.Expect(getErr).NotTo(HaveOccurred())
				g.Expect(phase).To(Equal("Succeeded"))
			}, 2*time.Minute, time.Second).Should(Succeed())
			output, err := metricsProbeOutput(rotatedProbe)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring("METRICS_OK"))

			afterPods, err := controllerPodIdentities()
			Expect(err).NotTo(HaveOccurred())
			Expect(afterPods).To(Equal(beforePods), "certificate reload must not recreate or restart controller pods")
		})

		It("should have CA injection for mutating webhooks", func() {
			By("checking CA injection for mutating webhooks")
			verifyCAInjection := func(g Gomega) {
				cmd := exec.Command("kubectl", "get",
					"mutatingwebhookconfigurations.admissionregistration.k8s.io",
					"sentry-kubernetes-operator-mutating-webhook-configuration",
					"-o", "go-template={{ range .webhooks }}{{ .clientConfig.caBundle }}{{ end }}")
				mwhOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(len(mwhOutput)).To(BeNumerically(">", 10))
			}
			Eventually(verifyCAInjection).Should(Succeed())
		})

		It("should have CA injection for validating webhooks", func() {
			By("checking CA injection for validating webhooks")
			verifyCAInjection := func(g Gomega) {
				cmd := exec.Command("kubectl", "get",
					"validatingwebhookconfigurations.admissionregistration.k8s.io",
					"sentry-kubernetes-operator-validating-webhook-configuration",
					"-o", "go-template={{ range .webhooks }}{{ .clientConfig.caBundle }}{{ end }}")
				vwhOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(len(vwhOutput)).To(BeNumerically(">", 10))
			}
			Eventually(verifyCAInjection).Should(Succeed())
		})

		It("should publish a redacted Event for a connection input transition", func() {
			const connectionName = "e2e-missing-secret"
			manifest := `apiVersion: sentryconfiguration.io/v1alpha1
kind: SentryConnection
metadata:
  name: e2e-missing-secret
spec:
  url: https://sentry.example.invalid
  organization: example
  credentials:
    tokenSecretRef:
      name: missing-e2e-token
      key: token
`
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "sentryconnection", connectionName, "--ignore-not-found"))
			})

			By("waiting for the redacted CredentialsReady condition")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "sentryconnection", connectionName,
					"-o", "jsonpath={.status.conditions[?(@.type=='CredentialsReady')].reason}")
				output, getErr := utils.Run(cmd)
				g.Expect(getErr).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("SecretInputInvalid"))
			}).Should(Succeed())

			By("waiting for the matching cluster-scoped object Event in the default namespace")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "events", "-n", "default",
					"--field-selector", "involvedObject.kind=SentryConnection,involvedObject.name="+connectionName,
					"-o", `jsonpath={range .items[?(@.reason=="SecretInputInvalid")]}{.message}{end}`)
				output, getErr := utils.Run(cmd)
				g.Expect(getErr).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("One or more referenced Secret inputs are unavailable or invalid."))
			}).Should(Succeed())
		})

		It("should preserve a live connection across a manager image rollout", func() {
			const connectionName = "e2e-upgrade-connection"
			By("waiting for the initial controller rollout before using admission")
			cmd := exec.Command("kubectl", "rollout", "status",
				"deployment/sentry-kubernetes-operator-controller-manager", "-n", namespace, "--timeout=3m")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			manifest := `apiVersion: sentryconfiguration.io/v1alpha1
kind: SentryConnection
metadata:
  name: e2e-upgrade-connection
spec:
  url: https://sentry.example.invalid
  organization: example
  credentials:
    tokenSecretRef:
      name: missing-upgrade-token
      key: token
`
			By("creating the live connection through the ready admission service")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "apply", "-f", "-")
				cmd.Stdin = strings.NewReader(manifest)
				_, applyErr := utils.Run(cmd)
				g.Expect(applyErr).NotTo(HaveOccurred())
			}, 30*time.Second, time.Second).Should(Succeed())
			DeferCleanup(func() {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "sentryconnection", connectionName, "--ignore-not-found"))
			})

			var connectionUID string
			By("waiting for the live connection condition before the rollout")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "sentryconnection", connectionName,
					"-o", "jsonpath={.metadata.uid}{'|'}{.status.conditions[?(@.type=='CredentialsReady')].reason}")
				output, getErr := utils.Run(cmd)
				g.Expect(getErr).NotTo(HaveOccurred())
				parts := strings.Split(output, "|")
				g.Expect(parts).To(HaveLen(2))
				g.Expect(parts[0]).NotTo(BeEmpty())
				g.Expect(parts[1]).To(Equal("SecretInputInvalid"))
				connectionUID = parts[0]
			}).Should(Succeed())

			By("creating protected pending-operation fixtures before the rollout")
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(protectedUpgradeFixtures)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(deleteProtectedUpgradeFixtures)
			protectedBefore, err := protectedFixtureSnapshots()
			Expect(err).NotTo(HaveOccurred())

			By("rolling the controller Deployment to the second image tag")
			cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", upgradeManagerImage))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			cmd = exec.Command("kubectl", "rollout", "status",
				"deployment/sentry-kubernetes-operator-controller-manager", "-n", namespace, "--timeout=3m")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the desired image and preserved connection identity")
			cmd = exec.Command("kubectl", "get", "deployment", "sentry-kubernetes-operator-controller-manager",
				"-n", namespace, "-o", `jsonpath={.spec.template.spec.containers[?(@.name=="manager")].image}`)
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal(upgradeManagerImage))
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "sentryconnection", connectionName,
					"-o", "jsonpath={.metadata.uid}{'|'}{.status.conditions[?(@.type=='CredentialsReady')].reason}")
				current, getErr := utils.Run(cmd)
				g.Expect(getErr).NotTo(HaveOccurred())
				g.Expect(current).To(Equal(connectionUID + "|SecretInputInvalid"))
			}).Should(Succeed())
			protectedAfterUpgrade, err := protectedFixtureSnapshots()
			Expect(err).NotTo(HaveOccurred())
			Expect(protectedAfterUpgrade).To(Equal(protectedBefore), "upgrade must preserve protected record UID, immutable flag, labels and payload bytes")

			By("verifying admission after the rollout")
			project := `apiVersion: sentryconfiguration.io/v1alpha1
kind: SentryProject
metadata:
  name: e2e-post-upgrade-admission
  namespace: default
spec:
  connection: e2e-upgrade-connection
  project:
    create:
      team: example
`
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "create", "--dry-run=server", "-f", "-")
				cmd.Stdin = strings.NewReader(project)
				_, createErr := utils.Run(cmd)
				g.Expect(createErr).NotTo(HaveOccurred())
			}, 30*time.Second, time.Second).Should(Succeed())

			By("rolling the controller Deployment back to the first image tag")
			cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			cmd = exec.Command("kubectl", "rollout", "status",
				"deployment/sentry-kubernetes-operator-controller-manager", "-n", namespace, "--timeout=3m")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			cmd = exec.Command("kubectl", "get", "deployment", "sentry-kubernetes-operator-controller-manager",
				"-n", namespace, "-o", `jsonpath={.spec.template.spec.containers[?(@.name=="manager")].image}`)
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal(managerImage))
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "sentryconnection", connectionName,
					"-o", "jsonpath={.metadata.uid}{'|'}{.status.conditions[?(@.type=='CredentialsReady')].reason}")
				current, getErr := utils.Run(cmd)
				g.Expect(getErr).NotTo(HaveOccurred())
				g.Expect(current).To(Equal(connectionUID + "|SecretInputInvalid"))
			}).Should(Succeed())
			protectedAfterRollback, err := protectedFixtureSnapshots()
			Expect(err).NotTo(HaveOccurred())
			Expect(protectedAfterRollback).To(Equal(protectedBefore), "rollback must preserve protected record UID, immutable flag, labels and payload bytes")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "create", "--dry-run=server", "-f", "-")
				cmd.Stdin = strings.NewReader(project)
				_, createErr := utils.Run(cmd)
				g.Expect(createErr).NotTo(HaveOccurred())
			}, 30*time.Second, time.Second).Should(Succeed())
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks

		// TODO: Customize the e2e test suite with scenarios specific to your project.
		// Consider applying sample/CR(s) and check their status and/or verifying
		// the reconciliation by using the metrics, i.e.:
		// metricsOutput, err := getMetricsOutput()
		// Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
		// Expect(metricsOutput).To(ContainSubstring(
		//    fmt.Sprintf(`controller_runtime_reconcile_total{controller="%s",result="success"} 1`,
		//    strings.ToLower(<Kind>),
		// ))
	})
})

type protectedFixtureReference struct {
	namespace string
	name      string
}

var protectedFixtureReferences = []protectedFixtureReference{
	{namespace: "sentry-kubernetes-operator-state", name: "e2e-pending-binding"},
	{namespace: "sentry-kubernetes-operator-state", name: "e2e-pending-operation"},
	{namespace: "sentry-kubernetes-operator-routing", name: "sentry-telegram-route-dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"},
	{namespace: "sentry-kubernetes-operator-delivery", name: "sentry-telegram-admission-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"},
	{namespace: "sentry-kubernetes-operator-delivery", name: "sentry-telegram-delivery-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	{namespace: "sentry-kubernetes-operator-delivery", name: "sentry-telegram-delivery-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
	{namespace: "sentry-kubernetes-operator-delivery", name: "sentry-telegram-delivery-cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"},
}

func protectedFixtureSnapshots() (map[string]string, error) {
	result := make(map[string]string, len(protectedFixtureReferences))
	for _, reference := range protectedFixtureReferences {
		output, err := utils.Run(exec.Command("kubectl", "get", "secret", reference.name, "-n", reference.namespace, "-o", "json"))
		if err != nil {
			return nil, err
		}
		var secret struct {
			Metadata struct {
				UID    string            `json:"uid"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Immutable bool              `json:"immutable"`
			Type      string            `json:"type"`
			Data      map[string]string `json:"data"`
		}
		if err := json.Unmarshal([]byte(output), &secret); err != nil {
			return nil, err
		}
		canonical, err := json.Marshal(secret)
		if err != nil {
			return nil, err
		}
		result[reference.namespace+"/"+reference.name] = string(canonical)
	}
	return result, nil
}

func deleteProtectedUpgradeFixtures() {
	for _, reference := range protectedFixtureReferences {
		_, _ = utils.Run(exec.Command("kubectl", "delete", "secret", reference.name, "-n", reference.namespace, "--ignore-not-found"))
	}
}

const protectedUpgradeFixtures = `apiVersion: v1
kind: Secret
metadata:
  name: e2e-pending-binding
  namespace: sentry-kubernetes-operator-state
  labels:
    app.kubernetes.io/managed-by: sentry-kubernetes-operator
    sentryconfiguration.io/record-type: project-binding
type: Opaque
stringData:
  state.json: '{"version":"v1","project":{"namespace":"default","name":"pending","uid":"e2e-pending-uid","generation":1},"connection":"company","installation":"https://sentry.example.invalid","organization":"example","slug":"pending","owner":"e2e","origin":"Managed","accessMode":"Manage","externalIdentity":"external-e2e","marker":{"origin":"managed","operatorOwner":"e2e","managedBy":"e2e-operator"},"managerUID":"e2e-pending-uid","claimName":"e2e-claim","pending":{"kind":"CreateProject","phase":"Submitted","desiredHash":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"},"pinnedAdapters":{"project":"legacy-24.7.1"}}'
---
apiVersion: v1
kind: Secret
metadata:
  name: e2e-pending-operation
  namespace: sentry-kubernetes-operator-state
  labels:
    app.kubernetes.io/managed-by: sentry-kubernetes-operator
    sentryconfiguration.io/record-type: operation-journal
type: Opaque
stringData:
  state.json: '{"Key":"e2e-operation","Kind":"CreateProject","LocalResource":"default/pending","IdempotencyHash":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff","Phase":"Submitted","Revision":1}'
---
apiVersion: v1
kind: Secret
metadata:
  name: sentry-telegram-route-dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
  namespace: sentry-kubernetes-operator-routing
  labels:
    sentryconfiguration.io/record-type: telegram-route
immutable: true
type: Opaque
stringData:
  route.json: '{"version":"v1","outboxKey":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","target":{"chatIdentity":"-100123","topicIdentity":"7"}}'
---
apiVersion: v1
kind: Secret
metadata:
  name: sentry-telegram-admission-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
  namespace: sentry-kubernetes-operator-delivery
  labels:
    sentryconfiguration.io/record-type: telegram-delivery-admission
immutable: true
type: Opaque
stringData:
  admission.json: '{"version":"v1alpha1","revision":1,"dedupeKey":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","messageHash":"4444444444444444444444444444444444444444444444444444444444444444","routeKey":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","text":"e2e protected admission payload","state":"Pending","attempts":0,"admissionPending":true,"createdAt":"2026-10-30T12:00:00Z","updatedAt":"2026-10-30T12:00:00Z","expiresAt":"2030-10-30T12:05:00Z"}'
---
apiVersion: v1
kind: Secret
metadata:
  name: sentry-telegram-delivery-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  namespace: sentry-kubernetes-operator-delivery
  labels:
    sentryconfiguration.io/record-type: telegram-delivery
    sentryconfiguration.io/delivery-state: InFlight
type: Opaque
stringData:
  delivery.json: '{"version":"v1alpha1","revision":2,"dedupeKey":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","messageHash":"1111111111111111111111111111111111111111111111111111111111111111","routeKey":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","text":"e2e protected in-flight payload","state":"InFlight","attempts":0,"claimOwner":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","leaseUntil":"2030-10-30T12:01:00Z","createdAt":"2026-10-30T12:00:00Z","updatedAt":"2026-10-30T12:00:00Z"}'
---
apiVersion: v1
kind: Secret
metadata:
  name: sentry-telegram-delivery-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  namespace: sentry-kubernetes-operator-delivery
  labels:
    sentryconfiguration.io/record-type: telegram-delivery
    sentryconfiguration.io/delivery-state: Retry
type: Opaque
stringData:
  delivery.json: '{"version":"v1alpha1","revision":3,"dedupeKey":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","messageHash":"2222222222222222222222222222222222222222222222222222222222222222","routeKey":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","text":"e2e protected retry payload","state":"Retry","attempts":1,"nextAttempt":"2030-10-30T12:01:00Z","lastFailure":"Unavailable","createdAt":"2026-10-30T12:00:00Z","updatedAt":"2026-10-30T12:00:00Z"}'
---
apiVersion: v1
kind: Secret
metadata:
  name: sentry-telegram-delivery-cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
  namespace: sentry-kubernetes-operator-delivery
  labels:
    sentryconfiguration.io/record-type: telegram-delivery
    sentryconfiguration.io/delivery-state: DeadLetter
type: Opaque
stringData:
  delivery.json: '{"version":"v1alpha1","revision":3,"dedupeKey":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","messageHash":"3333333333333333333333333333333333333333333333333333333333333333","routeKey":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","text":"e2e protected DLQ payload","state":"DeadLetter","attempts":1,"lastFailure":"Rejected","createdAt":"2026-10-30T12:00:00Z","updatedAt":"2026-10-30T12:00:00Z","expiresAt":"2030-11-30T12:00:00Z"}'
`

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() (string, error) {
	By("getting the curl-metrics logs")
	return metricsProbeOutput("curl-metrics")
}

func metricsProbeOutput(podName string) (string, error) {
	cmd := exec.Command("kubectl", "logs", podName, "-n", namespace)
	return utils.Run(cmd)
}

func createMetricsProbe(podName string) error {
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
spec:
  restartPolicy: Never
  serviceAccountName: %s
  containers:
  - name: curl
    image: curlimages/curl:8.16.0
    command: ["/bin/sh", "-c"]
    args:
    - 'token=$(cat /var/run/secrets/kubernetes.io/serviceaccount/token); for i in $(seq 1 30); do curl --silent --show-error --fail --cacert /metrics-ca/ca.crt --connect-timeout 2 --max-time 10 -H "Authorization: Bearer $token" https://%s.%s.svc.cluster.local:8443/metrics >/dev/null && echo METRICS_OK && exit 0 || sleep 2; done; exit 1'
    volumeMounts:
    - name: metrics-ca
      mountPath: /metrics-ca
      readOnly: true
    securityContext:
      readOnlyRootFilesystem: true
      allowPrivilegeEscalation: false
      capabilities:
        drop: ["ALL"]
      runAsNonRoot: true
      runAsUser: 1000
      seccompProfile:
        type: RuntimeDefault
  volumes:
  - name: metrics-ca
    secret:
      secretName: metrics-server-cert
      items:
      - key: ca.crt
        path: ca.crt
`, podName, namespace, serviceAccountName, metricsServiceName, namespace)
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	_, err := utils.Run(cmd)
	return err
}

func controllerPodIdentities() ([]string, error) {
	cmd := exec.Command("kubectl", "get", "pods", "-n", namespace,
		"-l", "control-plane=controller-manager",
		"-o", `jsonpath={range .items[*]}{.metadata.uid}{":"}{.status.containerStatuses[?(@.name=="manager")].restartCount}{"\n"}{end}`)
	output, err := utils.Run(cmd)
	if err != nil {
		return nil, err
	}
	identities := utils.GetNonEmptyLines(output)
	sort.Strings(identities)
	return identities, nil
}
