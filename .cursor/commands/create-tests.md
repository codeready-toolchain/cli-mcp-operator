---
name: create-tests
description: Write tests for recent code changes following project conventions
---

# Writing Tests for Recent Changes

Analyze the recent code changes (staged, unstaged, or described by the user) and write tests that cover the new or modified behavior. Follow the guidelines below.

## Running Tests

### From Project Root

- `make test` — Run all unit tests (`go test -v ./...`)
- `make test-coverage` — Run tests with coverage report
- `make lint` — Run golangci-lint
- `make build` — Build server and agent binaries

### Direct Go Commands

- **All tests**: `go test -v ./...`
- **Specific package**: `go test ./pkg/sandbox/... -v`
- **Specific test**: `go test ./pkg/sandbox -run TestCrashRecovery -v`
- **Specific subtest**: `go test ./pkg/sandbox -run "TestTimeout/returns_137" -v`
- **With race detector**: `go test -race ./... -v`

## Critical Rules

### 1. Be Pragmatic
Write tests that validate meaningful behavior. If a test requires excessive setup or doesn't test real logic, skip it. Prefer testing through the public API before resorting to white-box internals.

### 2. Test Behavior, Not Implementation
Focus on inputs, outputs, and observable side effects — not internal method call sequences. Assert on results returned from public functions.

### 3. No Historical Comments in Code
No `// Added in PR-123` or `// Testing new feature`. Historical context belongs in git commits. Tests should be self-explanatory.

### 4. Extend Existing Tests Before Creating New Ones
Each test file covers a specific package or concern. When adding test coverage, first check if an existing test file and test function already covers that area. Add subtests via `t.Run()` to existing functions rather than creating new top-level test functions that duplicate setup.

### 5. All Tests Must Pass
New tests are done ONLY when they ALL pass 100%. Don't leave failing tests. If a test is too complex to get right, don't write it.

### 6. No Documentation Files Unless Explicitly Requested
DO NOT create summary documents, README files, or any markdown documentation files unless the user explicitly asks for them. Focus exclusively on creating the actual test code.

## Project Conventions

- **Test file placement**: Tests live alongside code: `bash.go` → `bash_test.go`
- **Same-package tests**: Tests use `package sandbox` (not `sandbox_test`) to allow white-box access when needed
- **Assertions**: `testify/require` for fatal setup errors, `testify/assert` for value comparisons
- **Test helpers**: Mark with `t.Helper()`, use `t.Cleanup()` for teardown
- **Table-driven tests**: Standard Go pattern with `t.Run(tt.name, ...)` for parameterized scenarios

## Test Helper Pattern

Follow the existing `newTestSession` pattern for helpers:

```go
func newTestSession(t *testing.T) *BashSession {
	t.Helper()
	bs, err := NewBashSession(NewDefaultBashConfig())
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := bs.Close(); err != nil {
			t.Errorf("BashSession.Close: %v", err)
		}
	})
	return bs
}
```

## Test Structure Pattern

```go
func TestFeatureName(t *testing.T) {
	bs := newTestSession(t)

	// For simple cases — direct assertions
	res, err := bs.Execute("echo hello", DefaultTimeout)
	require.NoError(t, err)
	assert.Equal(t, "hello", res.Stdout)
	assert.Equal(t, 0, res.ExitCode)
}

// For parameterized cases — table-driven
func TestExitCodes(t *testing.T) {
	bs := newTestSession(t)

	tests := []struct {
		name     string
		command  string
		wantCode int
	}{
		{"success", "true", 0},
		{"failure", "false", 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := bs.Execute(tt.command, DefaultTimeout)
			require.NoError(t, err)
			assert.Equal(t, tt.wantCode, res.ExitCode)
		})
	}
}

// For related scenarios — subtests
func TestTimeout(t *testing.T) {
	bs := newTestSession(t)

	t.Run("returns 137 and timed out message", func(t *testing.T) {
		res, err := bs.Execute("sleep 30", 1*time.Second)
		require.NoError(t, err)
		assert.Equal(t, 137, res.ExitCode)
		assert.Contains(t, res.Stderr, "command timed out")
	})

	t.Run("session survives after timeout", func(t *testing.T) {
		res, err := bs.Execute("echo alive", DefaultTimeout)
		require.NoError(t, err)
		assert.Equal(t, "alive", res.Stdout)
	})
}
```

---

**BE PRACTICAL. CREATE ONLY TESTS THAT BRING REAL VALUE. DO NOT DUPLICATE TESTS AT THE SAME LEVEL.**

**DO NOT CREATE DOCUMENTATION OR SUMMARY FILES UNLESS EXPLICITLY REQUESTED.**

**Now create tests for the new functionality following the above guidelines.**
