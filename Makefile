# Image URL to use all building/pushing image targets
IMG ?= cli-mcp-operator:latest
SERVER_IMG ?= quay.io/codeready-toolchain/cli-mcp-server:latest
SANDBOX_IMG ?= quay.io/codeready-toolchain/cli-mcp-sandbox:latest
KUBE_RBAC_PROXY_IMG ?= quay.io/brancz/kube-rbac-proxy:v0.19.1

# VERSION defines the project version for the bundle.
VERSION ?= 0.0.1

ifneq ($(origin CHANNELS), undefined)
BUNDLE_CHANNELS := --channels=$(CHANNELS)
endif
ifneq ($(origin DEFAULT_CHANNEL), undefined)
BUNDLE_DEFAULT_CHANNEL := --default-channel=$(DEFAULT_CHANNEL)
endif
BUNDLE_METADATA_OPTS ?= $(BUNDLE_CHANNELS) $(BUNDLE_DEFAULT_CHANNEL)

IMAGE_TAG_BASE ?= quay.io/codeready-toolchain/cli-mcp-operator
BUNDLE_IMG ?= $(IMAGE_TAG_BASE)-bundle:v$(VERSION)
BUNDLE_GEN_FLAGS ?= -q --overwrite --version $(VERSION) $(BUNDLE_METADATA_OPTS)
BUNDLE_CSV = bundle/manifests/cli-mcp-operator.clusterserviceversion.yaml

USE_IMAGE_DIGESTS ?= false
ifeq ($(USE_IMAGE_DIGESTS), true)
	BUNDLE_GEN_FLAGS += --use-image-digests
endif

# Tool versions: claw-operator pins, operator-sdk bumped to latest 1.42.x.
OPERATOR_SDK_VERSION ?= v1.42.3
OPM_VERSION ?= v1.59.0
KIND_VERSION ?= v0.31.0
KUSTOMIZE_VERSION ?= v5.6.0
CONTROLLER_TOOLS_VERSION ?= v0.19.0
GOLANGCI_LINT_VERSION ?= v2.11.4

CONTAINER_TOOL ?= podman

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

include ./make/git.mk

GO_PACKAGE_ORG_NAME ?= codeready-toolchain
GO_PACKAGE_REPO_NAME ?= cli-mcp-operator
GO_PACKAGE_PATH ?= github.com/${GO_PACKAGE_ORG_NAME}/${GO_PACKAGE_REPO_NAME}
LDFLAGS = -X ${GO_PACKAGE_PATH}/pkg/version.commitOverride=${GIT_COMMIT_ID} -X ${GO_PACKAGE_PATH}/pkg/version.BuildTime=${BUILD_TIME}

# controller-gen must not scan pkg/ (data plane) or cmd/server / cmd/agent.
CONTROLLER_GEN_PATHS = paths="./api/..." paths="./internal/..." paths="./cmd/operator/..."

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate CRDs and operator manager-role into config/.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook $(CONTROLLER_GEN_PATHS) output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate DeepCopy methods.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" $(CONTROLLER_GEN_PATHS)

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate setup-envtest ## Run unit + envtest (exclude test/e2e). Covers pkg/ and internal/.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
		go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

.PHONY: test-coverage
test-coverage: test
	go tool cover -html=cover.out -o coverage.html

KIND_CLUSTER ?= cli-mcp-operator-test-e2e

.PHONY: setup-test-e2e
setup-test-e2e: kind ## Set up a Kind cluster for e2e tests if it does not exist.
	@case "$$($(KIND) get clusters)" in \
		*"$(KIND_CLUSTER)"*) \
			echo "Kind cluster '$(KIND_CLUSTER)' already exists. Skipping creation." ;; \
		*) \
			echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; \
			$(KIND) create cluster --name $(KIND_CLUSTER) ;; \
	esac

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate ## Run Kind e2e (harness only in Phase 2).
	KIND_CLUSTER=$(KIND_CLUSTER) go test -tags e2e ./test/e2e/ -v -ginkgo.v -timeout 15m

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the Kind cluster used for e2e tests.
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter.
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes.
	$(GOLANGCI_LINT) run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration.
	$(GOLANGCI_LINT) config verify

##@ Build

.PHONY: build
build: build-operator build-server build-agent ## Build manager + server + agent.

.PHONY: build-operator
build-operator: ## Build the operator manager binary (bin/manager).
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/manager ./cmd/operator

.PHONY: build-server
build-server: ## Build the MCP server binary.
	@echo "Building server (commit: $(GIT_COMMIT_ID_SHORT))..."
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/server -v ./cmd/server

.PHONY: build-agent
build-agent: ## Build the sandbox agent binary.
	@echo "Building agent (commit: $(GIT_COMMIT_ID_SHORT))..."
	@mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/agent -v ./cmd/agent

.PHONY: run
run: manifests generate ## Run the operator from your host.
	go run ./cmd/operator

.PHONY: run-server
run-server: ## Run the MCP server (data plane).
	go run ./cmd/server

.PHONY: run-agent
run-agent: ## Run the sandbox agent.
	go run ./cmd/agent

.PHONY: deps
deps:
	go mod download
	go mod tidy

.PHONY: clean
clean:
	go clean
	rm -rf bin cover.out coverage.html

##@ Container

.PHONY: container-build
container-build: ## Build the operator image (-f Containerfile.operator).
	$(CONTAINER_TOOL) build -f Containerfile.operator \
		--build-arg GIT_COMMIT=$(GIT_COMMIT_ID) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t $(IMG) .

.PHONY: container-build-server
container-build-server: ## Build the MCP server image.
	$(CONTAINER_TOOL) build -f Containerfile.server \
		--build-arg GIT_COMMIT=$(GIT_COMMIT_ID) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t $(SERVER_IMG) .

.PHONY: container-build-agent
container-build-agent: ## Build the sandbox agent image.
	$(CONTAINER_TOOL) build -f Containerfile.agent \
		--build-arg GIT_COMMIT=$(GIT_COMMIT_ID) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t $(SANDBOX_IMG) .

.PHONY: image-server
image-server: container-build-server

.PHONY: image-agent
image-agent: container-build-agent

.PHONY: container-push
container-push: ## Push the operator image.
	$(CONTAINER_TOOL) push $(IMG)

.PHONY: docker-build
docker-build: container-build ## Alias used by the Kind e2e harness.

.PHONY: docker-push
docker-push: container-push

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

# Temporary overlay so make deploy / make bundle do not mutate committed kustomization.yaml.
define generate-deploy-overlay
	@rm -rf config/.deploy && mkdir -p config/.deploy
	@img=$(1); printf 'apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- ../default\nimages:\n- name: controller\n  newName: %s\n  newTag: "%s"\npatches:\n- path: related_images_patch.yaml\n  target:\n    kind: Deployment\n' "$${img%:*}" "$${img##*:}" > config/.deploy/kustomization.yaml
	@printf 'apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: controller-manager\nspec:\n  template:\n    spec:\n      containers:\n      - name: manager\n        env:\n        - name: RELATED_IMAGE_SERVER\n          value: "$(2)"\n        - name: RELATED_IMAGE_SANDBOX\n          value: "$(3)"\n        - name: RELATED_IMAGE_KUBE_RBAC_PROXY\n          value: "$(4)"\n' > config/.deploy/related_images_patch.yaml
endef

define generate-bundle-overlay
	@rm -rf config/.bundle && mkdir -p config/.bundle
	@img=$(1); printf 'apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- ../manifests\nimages:\n- name: controller\n  newName: %s\n  newTag: "%s"\n' \
		"$${img%:*}" "$${img##*:}" > config/.bundle/kustomization.yaml
endef

.PHONY: install
install: manifests kustomize ## Install CRDs into the cluster in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the cluster in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy the operator without mutating committed kustomize files.
	$(call generate-deploy-overlay,$(IMG),$(SERVER_IMG),$(SANDBOX_IMG),$(KUBE_RBAC_PROXY_IMG))
	@trap 'rm -rf config/.deploy' EXIT; $(KUSTOMIZE) build config/.deploy | $(KUBECTL) apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy the operator from the cluster in ~/.kube/config.
	$(KUSTOMIZE) build config/default | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

KUBECTL ?= kubectl
KIND ?= $(LOCALBIN)/kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
OPERATOR_SDK ?= $(LOCALBIN)/operator-sdk
OPM ?= $(LOCALBIN)/opm

ENVTEST_VERSION ?= $(shell go list -m -f "{{ .Version }}" sigs.k8s.io/controller-runtime | awk -F'[v.]' '{printf "release-%d.%d", $$2, $$3}')
ENVTEST_K8S_VERSION ?= $(shell go list -m -f "{{ .Version }}" k8s.io/api | awk -F'[v.]' '{printf "1.%d", $$3}')

.PHONY: kustomize
kustomize: $(KUSTOMIZE)
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN)
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST)
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT)
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

.PHONY: operator-sdk
operator-sdk: $(OPERATOR_SDK)
$(OPERATOR_SDK): $(LOCALBIN)
	$(call download-tool,$(OPERATOR_SDK),$(OPERATOR_SDK_VERSION),\
		https://github.com/operator-framework/operator-sdk/releases/download/$(OPERATOR_SDK_VERSION)/operator-sdk_$(OS)_$(ARCH))

.PHONY: opm
opm: $(OPM)
$(OPM): $(LOCALBIN)
	$(call download-tool,$(OPM),$(OPM_VERSION),\
		https://github.com/operator-framework/operator-registry/releases/download/$(OPM_VERSION)/$(OS)-$(ARCH)-opm)

.PHONY: kind
kind: $(KIND)
$(KIND): $(LOCALBIN)
	$(call download-tool,$(KIND),$(KIND_VERSION),\
		https://github.com/kubernetes-sigs/kind/releases/download/$(KIND_VERSION)/kind-$(OS)-$(ARCH))

OS ?= $(shell go env GOOS)
ARCH ?= $(shell go env GOARCH)

define download-tool
@[ -f "$(1)-$(2)" ] || { \
set -e; \
echo "Downloading $(1) $(2)"; \
curl --silent --show-error --location --fail --retry 3 --output $(1)-$(2) $(3); \
chmod +x $(1)-$(2); \
}; \
ln -sf $(1)-$(2) $(1)
endef

define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef

##@ Bundle / catalog (catalog publish is Phase 4 CD)

.PHONY: bundle
bundle: manifests kustomize operator-sdk ## Generate bundle manifests with REPLACE_* relatedImages.
	$(OPERATOR_SDK) generate kustomize manifests -q
	$(call generate-bundle-overlay,$(IMG))
	$(KUSTOMIZE) build config/.bundle | $(OPERATOR_SDK) generate bundle $(BUNDLE_GEN_FLAGS)
	@rm -rf config/.bundle
	sed -i 's|image: $(IMG)|image: REPLACE_OPERATOR_IMAGE|' $(BUNDLE_CSV)
	sed -i 's|value: $(SERVER_IMG)|value: REPLACE_SERVER_IMAGE|' $(BUNDLE_CSV)
	sed -i 's|value: $(SANDBOX_IMG)|value: REPLACE_SANDBOX_IMAGE|' $(BUNDLE_CSV)
	sed -i 's|value: $(KUBE_RBAC_PROXY_IMG)|value: REPLACE_KUBE_RBAC_PROXY_IMAGE|' $(BUNDLE_CSV)
	sed -i 's|^    createdAt: .*|    createdAt: "REPLACE_CREATED_AT"|' $(BUNDLE_CSV)
	python3 -c 'import pathlib,re; p=pathlib.Path("$(BUNDLE_CSV)"); t=p.read_text(); t=re.sub(r"(?m)^  relatedImages:\n(?:  - image: .*\n    name: .*\n)+", "", t); b="  relatedImages:\n  - image: REPLACE_OPERATOR_IMAGE\n    name: manager\n  - image: REPLACE_SERVER_IMAGE\n    name: server\n  - image: REPLACE_SANDBOX_IMAGE\n    name: sandbox\n  - image: REPLACE_KUBE_RBAC_PROXY_IMAGE\n    name: kube-rbac-proxy\n"; t=re.sub(r"(?m)^  version: ", b+"  version: ", t, count=1); p.write_text(t)'
	$(OPERATOR_SDK) bundle validate ./bundle

.PHONY: bundle-build
bundle-build: ## Build the bundle image.
	$(CONTAINER_TOOL) build -f bundle.Dockerfile -t $(BUNDLE_IMG) .

.PHONY: bundle-push
bundle-push: ## Push the bundle image.
	$(MAKE) container-push IMG=$(BUNDLE_IMG)

.PHONY: catalog-build
# Phase 4 (SANDBOX-1983): switch to `opm render` before catalog CD; do not publish from master yet.
catalog-build: opm ## Build a catalog image (not published from master in Phase 2).
	$(OPM) index add --container-tool $(CONTAINER_TOOL) --mode semver --tag $(IMAGE_TAG_BASE)-catalog:v$(VERSION) --bundles $(BUNDLE_IMG)

.PHONY: catalog-push
catalog-push: ## Push a catalog image.
	$(MAKE) container-push IMG=$(IMAGE_TAG_BASE)-catalog:v$(VERSION)
