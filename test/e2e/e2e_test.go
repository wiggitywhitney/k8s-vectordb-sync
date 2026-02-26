//go:build e2e
// +build e2e

/*
Copyright 2026.

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
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/wiggitywhitney/k8s-vectordb-sync/test/utils"
)

// namespace where the project is deployed in
const namespace = "k8s-vectordb-sync-system"

// serviceAccountName created for the project
const serviceAccountName = "k8s-vectordb-sync-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "k8s-vectordb-sync-controller-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "k8s-vectordb-sync-metrics-binding"

// Pipeline test constants
const (
	mockServerPort     = "38080"
	mockServerLocalURL = "http://localhost:38080"
	testNamespace      = "e2e-pipeline-test"

	crdMockServerPort     = "38081"
	crdMockServerLocalURL = "http://localhost:38081"
)

// syncPayload mirrors the controller's SyncPayload for deserializing mock server responses.
type syncPayload struct {
	Upserts []resourceInstance `json:"upserts"`
	Deletes []string           `json:"deletes"`
}

// crdSyncPayload mirrors the controller's CrdSyncPayload for deserializing mock server responses.
type crdSyncPayload struct {
	Added   []string `json:"added"`
	Deleted []string `json:"deleted"`
}

// resourceInstance mirrors the controller's ResourceInstance for deserializing mock server responses.
type resourceInstance struct {
	ID          string            `json:"id"`
	Namespace   string            `json:"namespace"`
	Name        string            `json:"name"`
	Kind        string            `json:"kind"`
	APIVersion  string            `json:"apiVersion"`
	APIGroup    string            `json:"apiGroup"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	CreatedAt   string            `json:"createdAt"`
}

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
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the mock server")
		cmd = exec.Command("kubectl", "apply", "-f", "test/e2e/mockserver/manifests.yaml")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy mock server")

		By("waiting for mock server to be ready")
		verifyMockServerReady := func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "pods", "-l", "app=mock-server",
				"-n", "default", "-o", "jsonpath={.items[0].status.conditions[?(@.type=='Ready')].status}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("True"), "Mock server pod not ready")
		}
		Eventually(verifyMockServerReady, 2*time.Minute, time.Second).Should(Succeed())

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")

		By("configuring controller to use mock server with fast debounce")
		cmd = exec.Command("kubectl", "set", "env",
			"deployment/k8s-vectordb-sync-controller-manager",
			"-n", namespace,
			"INSTANCES_ENDPOINT=http://mock-server.default.svc:8080/api/v1/instances/sync",
			"CAPABILITIES_ENDPOINT=http://mock-server.default.svc:8080/api/v1/capabilities/scan",
			"DEBOUNCE_WINDOW_MS=2000",
			"BATCH_FLUSH_INTERVAL_MS=2000",
			"WATCH_RESOURCE_TYPES=deployments",
		)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to configure controller env vars")
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		By("cleaning up the curl pod for metrics")
		cmd := exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace)
		_, _ = utils.Run(cmd)

		By("cleaning up the metrics ClusterRoleBinding")
		cmd = exec.Command("kubectl", "delete", "clusterrolebinding", metricsRoleBindingName, "--ignore-not-found")
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

		By("cleaning up mock server")
		cmd = exec.Command("kubectl", "delete", "-f", "test/e2e/mockserver/manifests.yaml", "--ignore-not-found")
		_, _ = utils.Run(cmd)

		By("cleaning up test namespace")
		cmd = exec.Command("kubectl", "delete", "ns", testNamespace, "--ignore-not-found")
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
				// Get the name of the controller-manager pod
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
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				// Validate the pod's status
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
				"--clusterrole=k8s-vectordb-sync-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

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

			// +kubebuilder:scaffold:e2e-metrics-webhooks-readiness

			By("creating the curl-metrics pod to access the metrics endpoint")
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:latest",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"command": ["/bin/sh", "-c"],
							"args": [
								"for i in $(seq 1 30); do curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics && exit 0 || sleep 2; done; exit 1"
							],
							"securityContext": {
								"readOnlyRootFilesystem": true,
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccountName": "%s"
					}
				}`, token, metricsServiceName, namespace, serviceAccountName))
			_, err = utils.Run(cmd)
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
				g.Expect(metricsOutput).To(SatisfyAny(
				ContainSubstring("< HTTP/1.1 200 OK"),
				ContainSubstring("< HTTP/2 200"),
			), "Expected HTTP 200 response from metrics endpoint")
			}
			Eventually(verifyMetricsAvailable, 2*time.Minute).Should(Succeed())
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks
	})

	Context("Pipeline", Ordered, func() {
		var portForwardCmd *exec.Cmd

		BeforeAll(func() {
			By("refreshing controller pod name after env var restart")
			verifyControllerUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)
				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]

				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}
			Eventually(verifyControllerUp, 3*time.Minute, time.Second).Should(Succeed())

			By("starting port-forward to mock server")
			portForwardCmd = exec.Command("kubectl", "port-forward",
				"svc/mock-server", fmt.Sprintf("%s:8080", mockServerPort),
				"-n", "default")
			dir, _ := utils.GetProjectDir()
			portForwardCmd.Dir = dir
			err := portForwardCmd.Start()
			Expect(err).NotTo(HaveOccurred(), "Failed to start port-forward to mock server")

			By("waiting for port-forward to be ready")
			Eventually(func() error {
				resp, err := http.Get(mockServerLocalURL + "/healthz")
				if err != nil {
					return err
				}
				resp.Body.Close()
				return nil
			}, 30*time.Second, time.Second).Should(Succeed())

			By("waiting for controller initial sync to settle")
			// The controller lists all existing Deployments on startup and sends them.
			// Wait for the debounce window + flush interval to complete.
			time.Sleep(10 * time.Second)

			By("clearing mock server payloads from initial sync")
			clearMockPayloads()
		})

		AfterAll(func() {
			if portForwardCmd != nil && portForwardCmd.Process != nil {
				_ = portForwardCmd.Process.Kill()
				_ = portForwardCmd.Wait()
			}
		})

		It("should detect a new Deployment and send an upsert payload", func() {
			By("creating a test namespace")
			cmd := exec.Command("kubectl", "create", "ns", testNamespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create test namespace")

			By("creating a test Deployment")
			cmd = exec.Command("kubectl", "create", "deployment", "nginx-e2e",
				"--image=nginx:latest", "-n", testNamespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create test Deployment")

			By("waiting for the controller to detect and sync the Deployment")
			Eventually(func(g Gomega) {
				payloads := getMockPayloads()
				found := false
				for _, p := range payloads {
					for _, u := range p.Upserts {
						if u.Name == "nginx-e2e" && u.Namespace == testNamespace {
							found = true
							g.Expect(u.Kind).To(Equal("Deployment"))
							g.Expect(u.APIVersion).To(Equal("apps/v1"))
							g.Expect(u.APIGroup).To(Equal("apps"))
							g.Expect(u.ID).To(ContainSubstring("nginx-e2e"))
							g.Expect(u.CreatedAt).NotTo(BeEmpty())
						}
					}
				}
				g.Expect(found).To(BeTrue(), "expected upsert payload for nginx-e2e Deployment")
			}, 60*time.Second, 2*time.Second).Should(Succeed())
		})

		It("should detect a deleted Deployment and send a delete payload", func() {
			By("clearing mock server payloads")
			clearMockPayloads()

			By("deleting the test Deployment")
			cmd := exec.Command("kubectl", "delete", "deployment", "nginx-e2e",
				"-n", testNamespace, "--wait=false")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete test Deployment")

			By("waiting for the controller to detect and sync the deletion")
			Eventually(func(g Gomega) {
				payloads := getMockPayloads()
				found := false
				for _, p := range payloads {
					for _, d := range p.Deletes {
						if strings.Contains(d, "nginx-e2e") {
							found = true
						}
					}
				}
				g.Expect(found).To(BeTrue(), "expected delete payload for nginx-e2e")
			}, 60*time.Second, 2*time.Second).Should(Succeed())
		})
	})

	Context("CRD Pipeline", Ordered, func() {
		var portForwardCmd *exec.Cmd

		BeforeAll(func() {
			By("refreshing controller pod name for CRD pipeline tests")
			verifyControllerUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)
				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]

				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"))
			}
			Eventually(verifyControllerUp, 3*time.Minute, time.Second).Should(Succeed())

			By("starting port-forward to mock server for CRD tests")
			portForwardCmd = exec.Command("kubectl", "port-forward",
				"svc/mock-server", fmt.Sprintf("%s:8080", crdMockServerPort),
				"-n", "default")
			dir, _ := utils.GetProjectDir()
			portForwardCmd.Dir = dir
			err := portForwardCmd.Start()
			Expect(err).NotTo(HaveOccurred(), "Failed to start port-forward to mock server")

			By("waiting for port-forward to be ready")
			Eventually(func() error {
				resp, err := http.Get(crdMockServerLocalURL + "/healthz")
				if err != nil {
					return err
				}
				resp.Body.Close()
				return nil
			}, 30*time.Second, time.Second).Should(Succeed())

			By("waiting for controller to settle after startup")
			time.Sleep(10 * time.Second)

			By("clearing CRD payloads from initial sync")
			clearCrdMockPayloads()
		})

		AfterAll(func() {
			if portForwardCmd != nil && portForwardCmd.Process != nil {
				_ = portForwardCmd.Process.Kill()
				_ = portForwardCmd.Wait()
			}

			By("cleaning up test CRD")
			cmd := exec.Command("kubectl", "delete", "-f", "test/e2e/testdata/test-crd.yaml", "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		It("should detect a new CRD and send an add payload", func() {
			By("installing a test CRD")
			cmd := exec.Command("kubectl", "apply", "-f", "test/e2e/testdata/test-crd.yaml")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to install test CRD")

			By("waiting for the controller to detect and sync the CRD add event")
			Eventually(func(g Gomega) {
				payloads := getCrdMockPayloads()
				found := false
				for _, p := range payloads {
					for _, name := range p.Added {
						if name == "widgets.e2etest.example.com" {
							found = true
						}
					}
				}
				g.Expect(found).To(BeTrue(), "expected CRD add payload for widgets.e2etest.example.com")
			}, 60*time.Second, 2*time.Second).Should(Succeed())
		})

		It("should detect a deleted CRD and send a delete payload", func() {
			By("clearing CRD payloads")
			clearCrdMockPayloads()

			By("deleting the test CRD")
			cmd := exec.Command("kubectl", "delete", "-f", "test/e2e/testdata/test-crd.yaml", "--wait=false")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to delete test CRD")

			By("waiting for the controller to detect and sync the CRD delete event")
			Eventually(func(g Gomega) {
				payloads := getCrdMockPayloads()
				found := false
				for _, p := range payloads {
					for _, name := range p.Deleted {
						if name == "widgets.e2etest.example.com" {
							found = true
						}
					}
				}
				g.Expect(found).To(BeTrue(), "expected CRD delete payload for widgets.e2etest.example.com")
			}, 60*time.Second, 2*time.Second).Should(Succeed())
		})
	})
})

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	// Temporary file to store the token request
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		// Execute kubectl command to create the token
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		// Parse the JSON output to extract the token
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() (string, error) {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	return utils.Run(cmd)
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}

// mockHTTPClient is used for all mock server HTTP calls with a timeout
// to prevent CI hangs if the port-forward breaks.
var mockHTTPClient = &http.Client{Timeout: 5 * time.Second}

// getMockPayloads retrieves all recorded payloads from the mock server via port-forward.
func getMockPayloads() []syncPayload {
	resp, err := mockHTTPClient.Get(mockServerLocalURL + "/payloads")
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get payloads: %v\n", err)
		return nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Failed to read payloads response: %v\n", err)
		return nil
	}

	var payloads []syncPayload
	if err := json.Unmarshal(body, &payloads); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Failed to parse payloads: %v (body: %s)\n", err, string(body))
		return nil
	}
	return payloads
}

// clearMockPayloads clears all recorded payloads from the mock server.
func clearMockPayloads() {
	req, err := http.NewRequest(http.MethodDelete, mockServerLocalURL+"/payloads", nil)
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Failed to create clear request: %v\n", err)
		return
	}
	resp, err := mockHTTPClient.Do(req)
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Failed to clear payloads: %v\n", err)
		return
	}
	resp.Body.Close()
}

// crdMockHTTPClient is used for CRD mock server HTTP calls with a timeout.
var crdMockHTTPClient = &http.Client{Timeout: 5 * time.Second}

// getCrdMockPayloads retrieves all recorded CRD payloads from the mock server via port-forward.
func getCrdMockPayloads() []crdSyncPayload {
	resp, err := crdMockHTTPClient.Get(crdMockServerLocalURL + "/crd-payloads")
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get CRD payloads: %v\n", err)
		return nil
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Failed to read CRD payloads response: %v\n", err)
		return nil
	}

	var payloads []crdSyncPayload
	if err := json.Unmarshal(body, &payloads); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Failed to parse CRD payloads: %v (body: %s)\n", err, string(body))
		return nil
	}
	return payloads
}

// clearCrdMockPayloads clears all recorded CRD payloads from the mock server.
func clearCrdMockPayloads() {
	req, err := http.NewRequest(http.MethodDelete, crdMockServerLocalURL+"/crd-payloads", nil)
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Failed to create CRD clear request: %v\n", err)
		return
	}
	resp, err := crdMockHTTPClient.Do(req)
	if err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "Failed to clear CRD payloads: %v\n", err)
		return
	}
	resp.Body.Close()
}
