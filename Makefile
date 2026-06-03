include ./make/*.mk
.PHONY: build build-server build-agent run test clean fmt lint deps build-prod help image-agent

# Application name
APP_NAME = cli-mcp-server

# Build variables
BUILD_DIR = bin

# Go package path for ldflags
GO_PACKAGE_ORG_NAME ?= codeready-toolchain
GO_PACKAGE_REPO_NAME ?= $(shell basename $$PWD)
GO_PACKAGE_PATH ?= github.com/${GO_PACKAGE_ORG_NAME}/${GO_PACKAGE_REPO_NAME}

LDFLAGS = -X ${GO_PACKAGE_PATH}/pkg/version.commitOverride=${GIT_COMMIT_ID} -X ${GO_PACKAGE_PATH}/pkg/version.BuildTime=${BUILD_TIME}

# Go parameters
GOCMD = go
GOBUILD = $(GOCMD) build
GOCLEAN = $(GOCMD) clean
GOTEST = $(GOCMD) test
GOMOD = $(GOCMD) mod
GOFMT = gofmt

# Build both binaries
build: build-server build-agent

# Build the MCP server (control plane)
build-server:
	@echo "Building server (commit: $(GIT_COMMIT_ID_SHORT))..."
	@mkdir -p $(BUILD_DIR)
	$(GOBUILD) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/server -v ./cmd/server

# Build the sandbox agent (data plane)
build-agent:
	@echo "Building agent (commit: $(GIT_COMMIT_ID_SHORT))..."
	@mkdir -p $(BUILD_DIR)
	$(GOBUILD) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/agent -v ./cmd/agent

# Run the server
run:
	$(GOCMD) run ./cmd/server

# Test the application
test:
	$(GOTEST) -v ./...

# Test with coverage
test-coverage:
	$(GOTEST) -v -coverprofile=coverage.out ./...
	$(GOCMD) tool cover -html=coverage.out

# Clean build artifacts
clean:
	$(GOCLEAN)
	rm -rf $(BUILD_DIR)
	rm -f coverage.out

# Format code
fmt:
	$(GOFMT) -s -w .

# Lint code (requires golangci-lint)
lint:
	golangci-lint run

# Download dependencies
deps:
	$(GOMOD) download
	$(GOMOD) tidy

# Build for production (both binaries, static, CGO disabled)
build-prod: build-prod-server build-prod-agent

build-prod-server:
	@echo "Building server for production (commit: $(GIT_COMMIT_ID_SHORT))..."
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux $(GOBUILD) -a -installsuffix cgo -ldflags '$(LDFLAGS) -extldflags "-static"' -o $(BUILD_DIR)/server ./cmd/server

build-prod-agent:
	@echo "Building agent for production (commit: $(GIT_COMMIT_ID_SHORT))..."
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux $(GOBUILD) -a -installsuffix cgo -ldflags '$(LDFLAGS) -extldflags "-static"' -o $(BUILD_DIR)/agent ./cmd/agent

image-agent:
	podman build -f Containerfile.agent -t cli-mcp-sandbox:latest .

# Help
help:
	@echo "Available targets:"
	@echo "  build            - Build both server and agent binaries"
	@echo "  build-server     - Build the MCP server binary"
	@echo "  build-agent      - Build the sandbox agent binary"
	@echo "  run              - Run the MCP server"
	@echo "  test             - Run tests"
	@echo "  test-coverage    - Run tests with coverage"
	@echo "  clean            - Clean build artifacts"
	@echo "  fmt              - Format code"
	@echo "  lint             - Lint code"
	@echo "  deps             - Download dependencies"
	@echo "  build-prod       - Build both binaries for production"
	@echo "  build-prod-server- Build server for production"
	@echo "  build-prod-agent - Build agent for production"
	@echo "  image-agent      - Build sandbox agent container image"
	@echo "  help             - Show this help"
