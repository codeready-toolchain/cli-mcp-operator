# cli-mcp-server - agent guide

MCP control plane in Go that gives agents a sandboxed bash shell inside per-session Kubernetes pods. Two binaries: `cmd/server` (MCP server) and `cmd/agent` (sandbox data plane). Do not conflate them.

Go 1.26+. Default branch: `master`. Architecture: `README.md`, `docs/architecture-overview.md`, `docs/design.md`.

## Commands

Never invent alternate package-manager commands if these work.

- Build both: `make build` (`make build-server`, `make build-agent`)
- Test: `make test`
- Lint: `make lint`
- Format: `make fmt`
- Vet: `make vet`
- Prod binaries: `make build-prod`
- Images: `make image-server`, `make image-agent`

## Skills

Project skills live in `.cursor/skills/` (symlinked as `.claude/skills/`). Load a skill when its description matches the task. Do not preload all of them.

## Testing

- Add or update tests for behavior changes.
- Run the narrowest test first, then `make test` for the full unit gate.
- Single test: `go test ./pkg/sandbox -run TestName -v`
- `/create-tests` has package conventions (`testify`, same-package tests, table-driven).

## Commits

- Never commit, amend, or push unless the user explicitly asks in this turn
- “Make the change”, “fix the tests”, or finishing a task is not permission to commit
- If it is unclear, leave changes uncommitted and ask

## Safety

- Do not commit secrets (HMAC keys, kubeconfigs, `.env`)
- Do not weaken sandbox isolation, HMAC `/exec` auth, RBAC, or NetworkPolicy without an explicit human request
- Ask before large architectural changes

## This file

- If a change makes a stated fact here wrong (commands, paths, versions, binary names, safety constraints), update that bullet in the same change.
- Do not add recap, new sections, or session learnings. Keep this file short.
