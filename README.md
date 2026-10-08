# cli-mcp-operator

Kubernetes operator that gives AI agents a sandboxed bash shell in per-session pods.

Agents call one tool, `bash`: pipes, redirects, and whatever CLIs are in the sandbox image. The MCP server is stateless. Session state lives in the pod. The sandbox’s only path out is a credential proxy, which replaces any token the shell presents with the investigation credential.

```mermaid
flowchart LR
  Client[MCP client] --> Server[MCP server]
  Server --> Sandbox[Sandbox pod]
  Sandbox --> Proxy[Credential proxy]
  Proxy --> API[Allowed targets]
```

- One `CliMcpInstance` is one sandbox class: image, proxy targets, and pool.
- The operator owns the Deployments, NetworkPolicies, warm pool, and idle cleanup. The MCP server claims or creates a pod and proxies the command.
- A `kubernetes` target gives the sandbox a dummy kubeconfig; the real credential stays on the proxy. An `allowlist` target reaches only the listed hosts and injects no credential.

How the pieces behave: [Architecture](docs/architecture.md).

## Install

OLM, or the kustomize manifests. Apply a custom resource. A `kubernetes` target also needs an investigation kubeconfig Secret (`cli-mcp-<name>-kubeconfig`, key `kubeconfig`). On generic Kubernetes, provide a TLS Secret for the MCP Service. When the instance is Ready, mint a client token from `status.clientServiceAccount`.

```yaml
apiVersion: cli-mcp.redhat.com/v1alpha1
kind: CliMcpInstance
metadata:
  name: oc
spec:
  proxy:
    targets:
      - type: kubernetes
```

## Tool

| Parameter | Required | Description |
|---|---|---|
| `command` | yes | Full bash |
| `timeout` | no | Seconds (default 60, max 300) |

Send `X-Session-ID` (an RFC 1123 label) so later calls reuse the same shell and `/workspace`. `DELETE /sessions/{id}` removes the sandbox. Otherwise the operator collects it after the idle timeout. A non-zero exit code is a tool result.

## Development

```bash
make build    # operator, server, agent, and proxy
make test
make lint
```

`make run` starts the operator. `make run-server` starts the MCP server alone.

## License

[Apache License 2.0](LICENSE)
