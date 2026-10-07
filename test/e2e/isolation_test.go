//go:build e2e

/*
Copyright 2026 CodeReady Toolchain.

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
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/codeready-toolchain/cli-mcp-operator/test/utils"
)

const (
	instanceOC        = "oc"
	instanceAllow     = "curlinst"
	instanceBad       = "bad"
	strayPodName      = "cli-mcp-e2e-stray"
	proxyManagedToken = "proxy-managed-token"
)

func registerInstanceTests() {
	var (
		investigationToken string
		stolenToken        string
		ocSandbox          string
		allowSandbox       string
	)

	BeforeAll(func() {
		By("creating investigation and stolen identities")
		applyManifest(identityManifest())
		investigationToken = createToken("investigation")
		stolenToken = createToken("stolen")
		assertIdentitySplit(investigationToken, stolenToken)

		By("creating kubeconfig and TLS secrets")
		createKubeconfigSecret("cli-mcp-oc-kubeconfig", investigationKubeconfig(investigationToken))
		createKubeconfigSecret("cli-mcp-bad-kubeconfig", invalidKubeconfig)
		createTLSSecrets("cli-mcp-oc-tls", "cli-mcp-curlinst-tls", "cli-mcp-bad-tls")

		By("creating CliMcpInstances")
		applyManifest(instanceManifest(instanceOC, kubernetesInstance))
		applyManifest(instanceManifest(instanceAllow, allowlistInstance))
		applyManifest(instanceManifest(instanceBad, badInstance))

		By("waiting for kubernetes and allowlist instances to be Ready")
		Eventually(func(g Gomega) {
			for _, name := range []string{instanceOC, instanceAllow} {
				status, err := instanceCondition(name, "status")
				g.Expect(err).NotTo(HaveOccurred())
				reason, _ := instanceCondition(name, "reason")
				g.Expect(status).To(Equal("True"), "%s reason=%s", name, reason)
			}
		}, 5*time.Minute, 2*time.Second).Should(Succeed())

		ocSandbox = sandboxPodName(instanceOC)
		allowSandbox = sandboxPodName(instanceAllow)
	})

	It("marks a valid instance Ready and an invalid kubeconfig not Ready", func() {
		status, err := instanceCondition(instanceOC, "status")
		Expect(err).NotTo(HaveOccurred())
		Expect(status).To(Equal("True"))

		status, err = instanceCondition(instanceAllow, "status")
		Expect(err).NotTo(HaveOccurred())
		Expect(status).To(Equal("True"))

		Eventually(func(g Gomega) {
			status, err := instanceCondition(instanceBad, "status")
			g.Expect(err).NotTo(HaveOccurred())
			reason, err := instanceCondition(instanceBad, "reason")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(status).To(Equal("False"))
			g.Expect(reason).To(Equal("KubeconfigInvalid"))
		}, 2*time.Minute, time.Second).Should(Succeed())
	})

	It("mounts a dummy kubeconfig and the proxy env", func() {
		dummy, err := kubectl("get", "configmap", "cli-mcp-oc-proxy", "-n", namespace, "-o", "jsonpath={.data.kubeconfig}")
		Expect(err).NotTo(HaveOccurred(), dummy)
		Expect(dummy).To(ContainSubstring(proxyManagedToken))
		Expect(dummy).To(ContainSubstring("https://kubernetes.default.svc"))
		Expect(dummy).NotTo(ContainSubstring(investigationToken))

		pod, err := kubectl("get", "pod", ocSandbox, "-n", namespace, "-o", "json")
		Expect(err).NotTo(HaveOccurred(), pod)
		Expect(pod).To(ContainSubstring(`"automountServiceAccountToken": false`))
		Expect(pod).To(ContainSubstring(`"name": "cli-mcp-oc-proxy"`))
		Expect(pod).To(ContainSubstring(`"value": "http://cli-mcp-oc-proxy:8080"`))
		Expect(pod).NotTo(ContainSubstring("cli-mcp-oc-kubeconfig"))

		out, err := sandboxExec(ocSandbox, "printenv HTTPS_PROXY")
		Expect(err).NotTo(HaveOccurred(), out)
		Expect(strings.TrimSpace(out)).To(Equal("http://cli-mcp-oc-proxy:8080"))

		out, err = sandboxExec(allowSandbox, "printenv HTTPS_PROXY")
		Expect(err).NotTo(HaveOccurred(), out)
		Expect(strings.TrimSpace(out)).To(Equal("http://cli-mcp-curlinst-proxy:8080"))
		_, err = sandboxExec(allowSandbox, "test ! -e /config/kubeconfig && test -f /etc/cli-mcp/proxy-ca/ca.crt")
		Expect(err).NotTo(HaveOccurred())
	})

	It("cannot reach the API when the proxy is cleared", func() {
		out, err := sandboxExec(ocSandbox, "getent hosts kubernetes.default.svc")
		Expect(err).NotTo(HaveOccurred(), out)
		fields := strings.Fields(out)
		Expect(fields).NotTo(BeEmpty())
		apiIP := fields[0]

		_, err = sandboxExec(ocSandbox, withoutProxy(
			"curl -k --max-time 3 --connect-timeout 3 "+shellQuote(httpsURL(apiIP))))
		Expect(err).To(HaveOccurred())

		_, err = sandboxExec(ocSandbox, withoutProxy(
			"oc --insecure-skip-tls-verify --request-timeout=3s --server="+shellQuote(httpsURL(apiIP))+
				" --token=unused get --raw=/readyz"))
		Expect(err).To(HaveOccurred())

		_, err = sandboxExec(ocSandbox, withoutProxy(
			"oc --insecure-skip-tls-verify --request-timeout=3s --server=https://kubernetes.default.svc --token=unused get --raw=/readyz"))
		Expect(err).To(HaveOccurred())
	})

	It("keeps a stolen token on the investigation identity", func() {
		out, err := sandboxExecWithToken(ocSandbox, stolenToken,
			`oc --token "$TOKEN" get configmaps -n `+namespace+` --request-timeout=20s`)
		Expect(err).NotTo(HaveOccurred(), out)

		out, err = sandboxExecWithToken(ocSandbox, stolenToken,
			`oc --token "$TOKEN" get secrets -n `+namespace+` --request-timeout=20s`)
		Expect(err).To(HaveOccurred())
		Expect(out).To(ContainSubstring("Forbidden"))

		code, body := proxyCurl(ocSandbox, stolenToken,
			"https://kubernetes.default.svc/api/v1/namespaces/"+namespace+"/configmaps")
		Expect(code).To(Equal("200"), body)

		code, body = proxyCurl(ocSandbox, stolenToken,
			"https://kubernetes.default.svc/api/v1/namespaces/"+namespace+"/secrets")
		Expect(code).To(Equal("403"), body)
		Expect(body).NotTo(ContainSubstring("path not allowed"))
	})

	It("denies pod exec paths at the proxy", func() {
		code, body := proxyCurl(ocSandbox, stolenToken,
			"https://kubernetes.default.svc/api/v1/namespaces/"+namespace+"/pods/p/exec")
		Expect(code).To(Equal("403"), body)
		Expect(body).To(ContainSubstring("path not allowed"))
	})

	It("refuses the proxy to pods that are not this instance's sandboxes", func() {
		applyManifest(strayPodManifest())
		DeferCleanup(func() {
			_, _ = kubectl("delete", "pod", strayPodName, "-n", namespace, "--ignore-not-found", "--wait=false")
		})
		Eventually(func(g Gomega) {
			phase, err := kubectl("get", "pod", strayPodName, "-n", namespace, "-o", "jsonpath={.status.phase}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(phase).To(Equal("Running"))
		}, 2*time.Minute, time.Second).Should(Succeed())

		_, err := kubectl("exec", "-n", namespace, strayPodName, "-c", "stray", "--",
			"curl", "--max-time", "5", "--connect-timeout", "3", "http://cli-mcp-oc-proxy:8080/")
		Expect(err).To(HaveOccurred())

		_, err = sandboxExec(allowSandbox, withoutProxy(
			"curl --max-time 5 --connect-timeout 3 http://cli-mcp-oc-proxy:8080/"))
		Expect(err).To(HaveOccurred())

		_, err = sandboxExec(ocSandbox, withoutProxy(
			"curl --max-time 5 --connect-timeout 3 http://cli-mcp-curlinst-proxy:8080/"))
		Expect(err).To(HaveOccurred())
	})

	It("denies egress to port 53 that is not cluster DNS", func() {
		out, err := sandboxExec(ocSandbox, "getent hosts kubernetes.default.svc")
		Expect(err).NotTo(HaveOccurred(), out)
		apiIP := strings.Fields(out)[0]

		out, err = sandboxExec(ocSandbox, withoutProxy("curl --max-time 3 --connect-timeout 3 http://192.0.2.1:53"))
		Expect(err).To(HaveOccurred())
		Expect(out).To(MatchRegexp(`(?i)timed out|timeout`))

		out, err = sandboxExec(ocSandbox, withoutProxy(
			"curl --max-time 3 --connect-timeout 3 "+shellQuote(httpHostPort(apiIP, "53"))))
		Expect(err).To(HaveOccurred())
		Expect(out).To(MatchRegexp(`(?i)timed out|timeout`))
	})

	It("allows the configured domain and rejects the Kubernetes API host", func() {
		// HTTPS_PROXY is already set. Do not follow redirects: the allowlist is example.com only.
		out, err := sandboxExec(allowSandbox, "curl -fsS -o /dev/null --max-time 20 --connect-timeout 10 https://example.com/")
		Expect(err).NotTo(HaveOccurred(), out)

		// curl reports the CONNECT status and discards the response body, so read that body with a second CONNECT.
		out, err = sandboxExec(allowSandbox, "curl -sv --max-time 20 --connect-timeout 10 https://kubernetes.default.svc/api")
		Expect(err).To(HaveOccurred(), out)
		Expect(out).To(ContainSubstring("403"), out)

		body, err := sandboxExec(allowSandbox, `bash -c 'exec 3<>/dev/tcp/cli-mcp-curlinst-proxy/8080 && printf "CONNECT kubernetes.default.svc:443 HTTP/1.1\r\nHost: kubernetes.default.svc:443\r\nConnection: close\r\n\r\n" >&3 && timeout 5 cat <&3'`)
		Expect(err).NotTo(HaveOccurred(), body)
		Expect(body).To(ContainSubstring("403"))
		Expect(body).To(ContainSubstring("domain not allowed"))
	})
}

const (
	kubernetesInstance = `
  replicas: 1
  sandbox:
    warmPoolSize: 1
  proxy:
    targets:
      - type: kubernetes
`
	allowlistInstance = `
  replicas: 1
  sandbox:
    warmPoolSize: 1
  proxy:
    targets:
      - type: allowlist
        domains:
          - example.com
`
	badInstance = `
  replicas: 1
  sandbox:
    warmPoolSize: 0
  proxy:
    targets:
      - type: kubernetes
`
	invalidKubeconfig = `apiVersion: v1
kind: Config
current-context: c1
clusters:
- name: c1
  cluster:
    server: https://api.example.com:6443
contexts:
- name: c1
  context:
    cluster: c1
    user: u1
users:
- name: u1
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: echo
`
)

func identityManifest() string {
	return fmt.Sprintf(`apiVersion: v1
kind: ServiceAccount
metadata:
  name: investigation
  namespace: %s
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: investigation
  namespace: %s
rules:
- apiGroups: [""]
  resources: ["configmaps"]
  verbs: ["get", "list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: investigation
  namespace: %s
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: investigation
subjects:
- kind: ServiceAccount
  name: investigation
  namespace: %s
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: stolen
  namespace: %s
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: stolen
  namespace: %s
rules:
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["get", "list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: stolen
  namespace: %s
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: stolen
subjects:
- kind: ServiceAccount
  name: stolen
  namespace: %s
`, namespace, namespace, namespace, namespace, namespace, namespace, namespace, namespace)
}

func instanceManifest(name, spec string) string {
	return fmt.Sprintf(`apiVersion: cli-mcp.redhat.com/v1alpha1
kind: CliMcpInstance
metadata:
  name: %s
  namespace: %s
spec:%s`, name, namespace, spec)
}

func strayPodManifest() string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 1001
    seccompProfile:
      type: RuntimeDefault
  containers:
  - name: stray
    image: %s
    imagePullPolicy: IfNotPresent
    command: ["sleep", "3600"]
    securityContext:
      allowPrivilegeEscalation: false
      runAsNonRoot: true
      runAsUser: 1001
      capabilities:
        drop: ["ALL"]
      seccompProfile:
        type: RuntimeDefault
`, strayPodName, namespace, sandboxImage)
}

func investigationKubeconfig(token string) string {
	ca, err := kubectlStdout("get", "configmap", "kube-root-ca.crt", "-n", "kube-system",
		"-o", `go-template={{index .data "ca.crt"}}`)
	Expect(err).NotTo(HaveOccurred(), ca)
	encoded := base64.StdEncoding.EncodeToString([]byte(strings.TrimSpace(ca)))
	return fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: kind
clusters:
- name: kind
  cluster:
    server: https://kubernetes.default.svc
    certificate-authority-data: %s
contexts:
- name: kind
  context:
    cluster: kind
    user: investigation
users:
- name: investigation
  user:
    token: %q
`, encoded, token)
}

func createKubeconfigSecret(name, contents string) {
	path := writeTemp(contents)
	defer os.Remove(path)
	out, err := kubectl("create", "secret", "generic", name, "-n", namespace, "--from-file=kubeconfig="+path)
	Expect(err).NotTo(HaveOccurred(), out)
}

func createTLSSecrets(names ...string) {
	dir, err := os.MkdirTemp("", "cli-mcp-e2e-tls")
	Expect(err).NotTo(HaveOccurred())
	defer os.RemoveAll(dir)
	crt := filepath.Join(dir, "tls.crt")
	key := filepath.Join(dir, "tls.key")
	out, err := utils.Run(exec.Command("openssl", "req", "-x509", "-newkey", "rsa:2048",
		"-keyout", key, "-out", crt, "-days", "1", "-nodes", "-subj", "/CN=cli-mcp-e2e"))
	Expect(err).NotTo(HaveOccurred(), out)
	for _, name := range names {
		created, createErr := kubectl("create", "secret", "tls", name, "-n", namespace, "--cert="+crt, "--key="+key)
		Expect(createErr).NotTo(HaveOccurred(), created)
	}
}

func createToken(sa string) string {
	out, err := kubectlStdout("create", "token", sa, "-n", namespace, "--duration=2h")
	Expect(err).NotTo(HaveOccurred(), out)
	token := strings.TrimSpace(out)
	Expect(token).NotTo(BeEmpty())
	Expect(strings.Contains(token, "\n")).To(BeFalse())
	return token
}

// kubectlStdout returns stdout only so a token or CA is not mixed with warnings.
func kubectlStdout(args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	dir, err := utils.GetProjectDir()
	if err != nil {
		return "", err
	}
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr = string(exitErr.Stderr)
		}
		return string(out), fmt.Errorf("%w: %s", err, stderr)
	}
	return string(out), nil
}

func assertIdentitySplit(investigation, stolen string) {
	// Kind's kubeconfig authenticates with a client certificate. Passing
	// --token does not replace that certificate, so the check uses a
	// token-only kubeconfig.
	out, err := kubectlWithToken(investigation, "get", "configmaps", "-n", namespace)
	Expect(err).NotTo(HaveOccurred(), out)

	out, err = kubectlWithToken(investigation, "get", "secrets", "-n", namespace)
	Expect(err).To(HaveOccurred())
	Expect(out).To(ContainSubstring("Forbidden"))

	out, err = kubectlWithToken(stolen, "get", "secrets", "-n", namespace)
	Expect(err).NotTo(HaveOccurred(), out)
}

func kubectlWithToken(token string, args ...string) (string, error) {
	path := writeTemp(tokenOnlyKubeconfig(token))
	defer os.Remove(path)
	return kubectl(append([]string{"--kubeconfig=" + path}, args...)...)
}

func tokenOnlyKubeconfig(token string) string {
	server, err := kubectlStdout("config", "view", "--minify", "--raw", "-o", "jsonpath={.clusters[0].cluster.server}")
	Expect(err).NotTo(HaveOccurred(), server)
	ca, err := kubectlStdout("config", "view", "--minify", "--raw", "-o", "jsonpath={.clusters[0].cluster.certificate-authority-data}")
	Expect(err).NotTo(HaveOccurred())
	return fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: kind
clusters:
- name: kind
  cluster:
    server: %s
    certificate-authority-data: %s
contexts:
- name: kind
  context:
    cluster: kind
    user: token
users:
- name: token
  user:
    token: %q
`, strings.TrimSpace(server), strings.TrimSpace(ca), token)
}

func applyManifest(manifest string) {
	path := writeTemp(manifest)
	defer os.Remove(path)
	out, err := kubectl("apply", "-n", namespace, "-f", path)
	Expect(err).NotTo(HaveOccurred(), out)
}

func writeTemp(contents string) string {
	f, err := os.CreateTemp("", "cli-mcp-e2e-*.yaml")
	Expect(err).NotTo(HaveOccurred())
	path := f.Name()
	_, err = f.WriteString(contents)
	closeErr := f.Close()
	Expect(err).NotTo(HaveOccurred())
	Expect(closeErr).NotTo(HaveOccurred())
	return path
}

func instanceCondition(name, field string) (string, error) {
	return kubectl("get", "climcpinstance", name, "-n", namespace, "-o",
		"jsonpath={.status.conditions[?(@.type==\"Ready\")]."+field+"}")
}

func sandboxPodName(instance string) string {
	out, err := kubectl("get", "pods", "-n", namespace,
		"-l", "cli-mcp.redhat.com/instance="+instance+",cli-mcp.redhat.com/component=sandbox",
		"-o", "jsonpath={.items[0].metadata.name}")
	Expect(err).NotTo(HaveOccurred(), out)
	Expect(out).NotTo(BeEmpty())
	return out
}

func kubectl(args ...string) (string, error) {
	return utils.Run(exec.Command("kubectl", args...))
}

func sandboxExec(pod, script string) (string, error) {
	return kubectl("exec", "-n", namespace, pod, "-c", "sandbox", "--", "/bin/sh", "-c", script)
}

func withoutProxy(script string) string {
	return "unset HTTP_PROXY HTTPS_PROXY http_proxy https_proxy; " + script
}

func proxyCurl(pod, token, rawURL string) (string, string) {
	script := `curl -sS --max-time 20 -o /tmp/e2e-body -w '%{http_code}' -H "Authorization: Bearer $TOKEN" ` +
		shellQuote(rawURL) + `
printf '\n'
cat /tmp/e2e-body`
	out, err := sandboxExecWithToken(pod, token, script)
	Expect(err).NotTo(HaveOccurred(), out)
	code, body, _ := strings.Cut(out, "\n")
	return strings.TrimSpace(code), body
}

func sandboxExecWithToken(pod, token, script string) (string, error) {
	cmd := exec.Command("kubectl", "exec", "-i", "-n", namespace, pod, "-c", "sandbox", "--",
		"/bin/sh", "-c", "read -r TOKEN; "+script)
	cmd.Stdin = strings.NewReader(token + "\n")
	dir, err := utils.GetProjectDir()
	if err != nil {
		return "", err
	}
	cmd.Dir = dir
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %q\n", strings.Join(cmd.Args, " "))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%q failed with error %q: %w", strings.Join(cmd.Args, " "), out, err)
	}
	return string(out), nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func httpsURL(host string) string {
	if strings.Contains(host, ":") {
		return "https://[" + host + "]"
	}
	return "https://" + host
}

func httpHostPort(host, port string) string {
	if strings.Contains(host, ":") {
		return "http://[" + host + "]:" + port
	}
	return "http://" + host + ":" + port
}
