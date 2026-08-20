package main

import (
	"os/exec"
	"strings"
	"testing"
)

// Phase 2 import boundary: data-plane packages must not pull in the operator.
// cmd/server and cmd/agent may depend on pkg/* only (not api/, internal/, or
// controller-runtime). pkg/session must stay CRD-agnostic.
func TestImportBoundary(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	dataPlane := []string{"./cmd/server", "./cmd/agent", "./pkg/..."}
	forbidden := []string{
		"cli-mcp-operator/internal",
		"cli-mcp-operator/api",
		"sigs.k8s.io/controller-runtime",
	}

	args := append([]string{"list", "-deps"}, dataPlane...)
	cmd := exec.CommandContext(t.Context(), "go", args...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps %s: %v\n%s", strings.Join(dataPlane, " "), err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		for _, pattern := range forbidden {
			if strings.Contains(line, pattern) {
				t.Errorf("%s depends on %s", strings.Join(dataPlane, " "), line)
			}
		}
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatalf("go list -m: %v", err)
	}
	return strings.TrimSpace(string(out))
}
