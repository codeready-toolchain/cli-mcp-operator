---
name: test-and-fix
description: Run build, lint, and tests — fix all issues until green
---

# Check and Fix

Run `make build`, `make lint`, and `make test` to find all issues, then fix them.

1. Run `make build` to catch compilation errors. Fix any that appear before proceeding.
2. Run `make lint` to surface lint issues. Fix anything reported.
3. Run `make test` to run all unit tests. For failures:
   - Read the failing test AND the code under test to understand the root cause
   - Fix the source code, not the tests — unless the test itself is wrong
   - Run the specific failing test to verify: `go test ./pkg/sandbox -run TestName -v`
4. Repeat steps 1–3 until all three commands pass cleanly.

Do not create documentation files or summaries. Focus exclusively on making the build green.
