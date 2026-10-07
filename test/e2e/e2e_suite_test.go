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
	"fmt"
	"os"
	"os/exec"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/codeready-toolchain/cli-mcp-operator/test/utils"
)

var (
	// Optional Environment Variables:
	// - CERT_MANAGER_INSTALL_SKIP=true: Skips CertManager installation during test setup.
	// These variables are useful if CertManager is already installed, avoiding
	// re-installation and conflicts.
	skipCertManagerInstall = os.Getenv("CERT_MANAGER_INSTALL_SKIP") == "true"
	// isCertManagerAlreadyInstalled will be set true when CertManager CRDs be found on the cluster
	isCertManagerAlreadyInstalled = false

	// Images are tagged with an explicit version so kubelet uses IfNotPresent
	// after they are loaded into Kind. :latest would always pull.
	projectImage       = "example.com/cli-mcp-operator:v0.0.1"
	serverImage        = "example.com/cli-mcp-server:v0.0.1"
	sandboxImage       = "example.com/cli-mcp-sandbox:v0.0.1"
	proxyImage         = "example.com/cli-mcp-proxy:v0.0.1"
	kubeRBACProxyImage = "quay.io/brancz/kube-rbac-proxy:v0.19.1"
)

// TestE2E runs the end-to-end (e2e) test suite for the project. These tests execute in an isolated,
// temporary environment to validate project changes with the purposed to be used in CI jobs.
// The default setup requires Kind, builds/loads the Manager Docker image locally, and installs
// CertManager.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting cli-mcp-operator integration test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	if os.Getenv("KIND_EXPERIMENTAL_PROVIDER") == "" && os.Getenv("CONTAINER_TOOL") != "" {
		Expect(os.Setenv("KIND_EXPERIMENTAL_PROVIDER", os.Getenv("CONTAINER_TOOL"))).To(Succeed())
	}

	By("building operator, server, sandbox, and proxy images")
	for _, args := range [][]string{
		{"container-build", "IMG=" + projectImage},
		{"container-build-server", "SERVER_IMG=" + serverImage},
		{"container-build-agent", "SANDBOX_IMG=" + sandboxImage},
		{"container-build-proxy", "PROXY_IMG=" + proxyImage},
	} {
		cmd := exec.Command("make", args...)
		_, err := utils.Run(cmd)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build %s", args[0])
	}

	tool := os.Getenv("CONTAINER_TOOL")
	if tool == "" {
		tool = "podman"
	}
	By("pulling kube-rbac-proxy")
	_, err := utils.Run(exec.Command(tool, "pull", kubeRBACProxyImage))
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to pull kube-rbac-proxy")

	By("loading images into Kind")
	for _, image := range []string{projectImage, serverImage, sandboxImage, proxyImage, kubeRBACProxyImage} {
		err = utils.LoadImageToKindClusterWithName(image)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load %s into Kind", image)
	}

	// The tests-e2e are intended to run on a temporary cluster that is created and destroyed for testing.
	// To prevent errors when tests run in environments with CertManager already installed,
	// we check for its presence before execution.
	// Setup CertManager before the suite if not skipped and if not already installed
	if !skipCertManagerInstall {
		By("checking if cert manager is installed already")
		isCertManagerAlreadyInstalled = utils.IsCertManagerCRDsInstalled()
		if !isCertManagerAlreadyInstalled {
			_, _ = fmt.Fprintf(GinkgoWriter, "Installing CertManager...\n")
			Expect(utils.InstallCertManager()).To(Succeed(), "Failed to install CertManager")
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "WARNING: CertManager is already installed. Skipping installation...\n")
		}
	}
})

var _ = AfterSuite(func() {
	// Teardown CertManager after the suite if not skipped and if it was not already installed
	if !skipCertManagerInstall && !isCertManagerAlreadyInstalled {
		_, _ = fmt.Fprintf(GinkgoWriter, "Uninstalling CertManager...\n")
		utils.UninstallCertManager()
	}
})
