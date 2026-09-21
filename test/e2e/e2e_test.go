//go:build e2e
// +build e2e

/*
Copyright 2025.

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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ripolin/klap/test/utils"
)

// namespace where the project is deployed in
const namespace = "klap-system"

// serviceAccountName created for the project
const serviceAccountName = "klap-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "klap-controller-manager-metrics-service"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "klap-metrics-binding"

// openldapNamespace is the namespace where the OpenLDAP instance installed by the suite lives.
const openldapNamespace = "openldap"

// openldapAdminDN and openldapAdminPassword are the admin credentials of the OpenLDAP instance
// installed by the suite (see config/openldap).
const (
	openldapAdminDN       = "cn=admin,dc=example,dc=org"
	openldapAdminPassword = "passwd"
)

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
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
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
				"--clusterrole=klap-metrics-reader",
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

			By("waiting for the webhook service endpoints to be ready")
			verifyWebhookEndpointsReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "endpointslices.discovery.k8s.io", "-n", namespace,
					"-l", "kubernetes.io/service-name=klap-webhook-service",
					"-o", "jsonpath={range .items[*]}{range .endpoints[*]}{.addresses[*]}{end}{end}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Webhook endpoints should exist")
				g.Expect(output).ShouldNot(BeEmpty(), "Webhook endpoints not yet ready")
			}
			Eventually(verifyWebhookEndpointsReady, 3*time.Minute, time.Second).Should(Succeed())

			By("verifying the mutating webhook server is ready")
			verifyMutatingWebhookReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "mutatingwebhookconfigurations.admissionregistration.k8s.io",
					"klap-mutating-webhook-configuration",
					"-o", "jsonpath={.webhooks[0].clientConfig.caBundle}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "MutatingWebhookConfiguration should exist")
				g.Expect(output).ShouldNot(BeEmpty(), "Mutating webhook CA bundle not yet injected")
			}
			Eventually(verifyMutatingWebhookReady, 3*time.Minute, time.Second).Should(Succeed())

			By("verifying the validating webhook server is ready")
			verifyValidatingWebhookReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "validatingwebhookconfigurations.admissionregistration.k8s.io",
					"klap-validating-webhook-configuration",
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
				g.Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
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

		It("should have CA injection for mutating webhooks", func() {
			By("checking CA injection for mutating webhooks")
			verifyCAInjection := func(g Gomega) {
				cmd := exec.Command("kubectl", "get",
					"mutatingwebhookconfigurations.admissionregistration.k8s.io",
					"klap-mutating-webhook-configuration",
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
					"klap-validating-webhook-configuration",
					"-o", "go-template={{ range .webhooks }}{{ .clientConfig.caBundle }}{{ end }}")
				vwhOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(len(vwhOutput)).To(BeNumerically(">", 10))
			}
			Eventually(verifyCAInjection).Should(Succeed())
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks

		Context("Server and Entry CRDs", Ordered, func() {
			const (
				serverName = "e2e-openldap-server"
				entryName  = "e2e-openldap-entry"
				entryDN    = "cn=e2e-joe,dc=example,dc=org"

				// crossNamespace hosts an Entry that lives outside the Server's namespace.
				// It must match the Server's allowedNamespaces name pattern.
				crossNamespace       = "e2e-entries"
				crossNamespaceEntry  = "e2e-cross-namespace-entry"
				crossNamespaceDN     = "cn=e2e-jane,dc=example,dc=org"
				allowedNamespaceExpr = "e2e-.*"
			)
			var serverManifestFile, entryManifestFile, crossNamespaceEntryManifestFile string

			AfterAll(func() {
				// Entries must go first: they reference the Server.
				for _, manifestFile := range []string{crossNamespaceEntryManifestFile, entryManifestFile, serverManifestFile} {
					if manifestFile == "" {
						continue
					}
					By(fmt.Sprintf("deleting %s", filepath.Base(manifestFile)))
					cmd := exec.Command("kubectl", "delete", "-f", manifestFile, "--ignore-not-found")
					_, _ = utils.Run(cmd)
					_ = os.Remove(manifestFile)
				}

				By(fmt.Sprintf("deleting namespace %s", crossNamespace))
				cmd := exec.Command("kubectl", "delete", "ns", crossNamespace, "--ignore-not-found")
				_, _ = utils.Run(cmd)
			})

			It("should create a Server CR pointing to the OpenLDAP instance installed by the suite", func() {
				By("creating a Server CR referencing the OpenLDAP service and secrets")
				serverManifest := fmt.Sprintf(`
apiVersion: klap.ripolin.github.com/v1alpha1
kind: Server
metadata:
  name: %s
  namespace: %s
spec:
  url: ldap://ldap.%s.svc.cluster.local
  baseDN: dc=example,dc=org
  bindDN: cn=admin,dc=example,dc=org
  implementation: openldap
  startTLS: true
  allowedNamespaces:
    namePattern: "%s"
  passwordSecretRef:
    name: openldap-passwd
    key: adminPassword
  tlsSecretRef:
    name: openldap-server-cert
    key: ca.crt
`, serverName, openldapNamespace, openldapNamespace, allowedNamespaceExpr)

				var err error
				serverManifestFile, err = applyManifest(serverName, serverManifest)
				Expect(err).NotTo(HaveOccurred(), "Failed to create Server CR")

				By("verifying the Server CR was accepted and its spec matches the OpenLDAP install")
				cmd := exec.Command("kubectl", "get", "server", serverName, "-n", openldapNamespace,
					"-o", "jsonpath={.spec.url}")
				output, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred(), "Failed to get Server CR")
				Expect(output).To(Equal(fmt.Sprintf("ldap://ldap.%s.svc.cluster.local", openldapNamespace)))
			})

			It("should create an Entry CR referencing the Server", func() {
				const entryDN = "cn=e2e-joe,dc=example,dc=org"

				By("creating an Entry CR referencing the Server CR")
				entryManifest := fmt.Sprintf(`
apiVersion: klap.ripolin.github.com/v1alpha1
kind: Entry
metadata:
  name: %s
  namespace: %s
spec:
  dn: %s
  prune: true
  force: false
  adopt: true
  attributes:
    objectClass:
      - inetOrgPerson
    sn:
      - Doe
    mail:
      - joe@example.org
  serverRef:
    name: %s
    namespace: %s
`, entryName, openldapNamespace, entryDN, serverName, openldapNamespace)

				var err error
				entryManifestFile, err = applyManifest(entryName, entryManifest)
				Expect(err).NotTo(HaveOccurred(), "Failed to create Entry CR")

				By("verifying the Entry CR was accepted and references the Server CR")
				cmd := exec.Command("kubectl", "get", "entry", entryName, "-n", openldapNamespace,
					"-o", "jsonpath={.spec.dn} {.spec.serverRef.name}")
				output, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred(), "Failed to get Entry CR")
				Expect(output).To(Equal(fmt.Sprintf("%s %s", entryDN, serverName)))

				By("verifying the Entry CR status is Available")
				Eventually(verifyEntryAvailable(entryName, openldapNamespace)).
					WithTimeout(time.Minute).Should(Succeed())
			})

			It("should create an Entry CR outside the Server's namespace", func() {
				const entryDN = "cn=e2e-jane,dc=example,dc=org"

				By("creating a namespace matching the Server's allowedNamespaces pattern")
				cmd := exec.Command("kubectl", "create", "ns", crossNamespace)
				_, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

				By("creating an Entry CR in that namespace referencing the Server CR")
				entryManifest := fmt.Sprintf(`
apiVersion: klap.ripolin.github.com/v1alpha1
kind: Entry
metadata:
  name: %s
  namespace: %s
spec:
  dn: %s
  prune: true
  force: false
  adopt: true
  attributes:
    objectClass:
      - inetOrgPerson
    sn:
      - Doe
    mail:
      - jane@example.org
  serverRef:
    name: %s
    namespace: %s
`, crossNamespaceEntry, crossNamespace, crossNamespaceDN, serverName, openldapNamespace)

				crossNamespaceEntryManifestFile, err = applyManifest(crossNamespaceEntry, entryManifest)
				Expect(err).NotTo(HaveOccurred(), "Failed to create cross-namespace Entry CR")

				By("verifying the Entry CR lives outside the Server's namespace and references it")
				cmd = exec.Command("kubectl", "get", "entry", crossNamespaceEntry, "-n", crossNamespace,
					"-o", "jsonpath={.spec.dn} {.spec.serverRef.name} {.spec.serverRef.namespace}")
				output, err := utils.Run(cmd)
				Expect(err).NotTo(HaveOccurred(), "Failed to get cross-namespace Entry CR")
				Expect(output).To(Equal(fmt.Sprintf("%s %s %s", crossNamespaceDN, serverName, openldapNamespace)))

				By("verifying the controller accepted the cross-namespace reference")
				Eventually(verifyEntryAvailable(crossNamespaceEntry, crossNamespace)).
					WithTimeout(time.Minute).Should(Succeed())
			})

			It("should delete the Entry CR and prune its LDAP entry", func() {
				deleteEntryAndVerifyPruned(entryName, openldapNamespace, entryDN)

				By("verifying the other Entry's LDAP entry was left untouched")
				exists, err := ldapEntryExists(crossNamespaceDN)
				Expect(err).NotTo(HaveOccurred())
				Expect(exists).To(BeTrue(), "LDAP entry %s should not have been pruned", crossNamespaceDN)
			})

			It("should delete the Entry CR outside the Server's namespace and prune its LDAP entry", func() {
				deleteEntryAndVerifyPruned(crossNamespaceEntry, crossNamespace, crossNamespaceDN)
			})
		})

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

// applyManifest writes the manifest to a temporary file named after the resource and applies it.
// It returns the file path so the caller can delete the resource and remove the file afterwards.
func applyManifest(name, manifest string) (string, error) {
	manifestFile := filepath.Join(os.TempDir(), fmt.Sprintf("%s.yaml", name))
	if err := os.WriteFile(manifestFile, []byte(manifest), 0o644); err != nil {
		return "", fmt.Errorf("writing manifest %s: %w", manifestFile, err)
	}
	cmd := exec.Command("kubectl", "apply", "-f", manifestFile)
	if _, err := utils.Run(cmd); err != nil {
		return manifestFile, err
	}
	return manifestFile, nil
}

// verifyEntryAvailable returns a check that the Entry has its Available condition set to True.
func verifyEntryAvailable(name, namespace string) func(Gomega) {
	return func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "entry", name, "-n", namespace,
			"-o", `jsonpath={.status.conditions[?(@.type=="Available")].status}`)
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(Equal("True"), "Entry %s/%s should be Available", namespace, name)
	}
}

// deleteEntryAndVerifyPruned deletes an Entry with prune enabled and checks that its finalizer
// was released (the resource disappears) and that the backing LDAP entry was removed.
func deleteEntryAndVerifyPruned(name, namespace, dn string) {
	By(fmt.Sprintf("verifying the LDAP entry %s exists before deletion", dn))
	exists, err := ldapEntryExists(dn)
	Expect(err).NotTo(HaveOccurred())
	Expect(exists).To(BeTrue(), "LDAP entry %s should exist before the Entry CR is deleted", dn)

	By(fmt.Sprintf("deleting the Entry CR %s/%s", namespace, name))
	// kubectl waits for the finalizer to be released before returning.
	cmd := exec.Command("kubectl", "delete", "entry", name, "-n", namespace, "--timeout=2m")
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to delete Entry CR")

	By("verifying the Entry CR is gone")
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "entry", name, "-n", namespace)
		output, err := utils.Run(cmd)
		g.Expect(err).To(HaveOccurred(), "Entry %s/%s should be deleted", namespace, name)
		g.Expect(output).To(ContainSubstring("NotFound"))
	}).WithTimeout(time.Minute).Should(Succeed())

	By(fmt.Sprintf("verifying the LDAP entry %s was pruned", dn))
	Eventually(func(g Gomega) {
		exists, err := ldapEntryExists(dn)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(exists).To(BeFalse(), "LDAP entry %s should have been pruned", dn)
	}).WithTimeout(time.Minute).Should(Succeed())
}

// ldapEntryExists reports whether dn exists in the OpenLDAP instance installed by the suite.
func ldapEntryExists(dn string) (bool, error) {
	cmd := exec.Command("kubectl", "exec", "-n", openldapNamespace, "statefulset/openldap", "--",
		"ldapsearch", "-x", "-LLL",
		"-H", "ldap://localhost:1389",
		"-D", openldapAdminDN, "-w", openldapAdminPassword,
		"-b", dn, "-s", "base", "dn")
	_, err := utils.Run(cmd)
	if err == nil {
		return true, nil
	}
	// ldapsearch reports a missing base object as "No such object (32)".
	if strings.Contains(err.Error(), "No such object") {
		return false, nil
	}
	return false, err
}

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	By("creating temporary file to store the token request")
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		By("executing kubectl command to create the token")
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		By("parsing the JSON output to extract the token")
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
