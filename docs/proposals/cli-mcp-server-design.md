# CLI MCP Server — Detailed Design

**Status:** Final — all decisions made.

**Prerequisite:** [Sketch document](cli-mcp-server-sketch.md) — contains problem statement, approach selection, and high-level decisions.

## Overview

A Go MCP server that provides TARSy investigation agents with a **sandboxed execution environment** — a per-investigation Kubernetes pod where the LLM has a persistent bash shell with full CLI access. The server exposes a single MCP tool — `bash` (command execution) — manages sandbox pod lifecycle via `client-go`, and proxies commands to a lightweight agent binary running inside each pod. Session cleanup is handled via a plain HTTP endpoint (`DELETE /sessions/{id}`), called by TARSy when the investigation completes.

The server runs in **stateless mode** (`--stateless`), consistent with all other MCP servers. Session routing uses the `X-Session-ID` HTTP header, injected automatically by TARSy on every MCP request, with Kubernetes pod labels as the source of truth. The LLM never sees or manages session IDs. Any MCP server replica can serve any request — no sticky sessions, no shared state.

**Initial scope:** `oc`, `kubectl`, and standard Unix utilities (`jq`, `yq`, `grep`, `awk`, `curl`). The sandbox image is extensible to additional CLIs by installing binaries — no server code changes.

## Design Principles

1. **Stateless server, stateful sandbox** — the MCP server is stateless (any replica, any request). All session state lives in the sandbox pod's persistent bash process and ephemeral filesystem.
2. **Sandbox as security boundary** — no application-level command filtering. Security comes from RBAC, pod isolation, network isolation, HMAC-based agent authentication, and ephemeral storage.
3. **Dumb proxy** — the MCP server's job is pod lifecycle management and command proxying. It doesn't parse, validate, or transform shell commands.
4. **Consistent with the ecosystem** — follows `mcp-server-devsandbox` patterns for middleware, deployment, stateless mode, and testing.
5. **Two simple binaries** — the MCP server (control plane) and the sandbox agent (data plane) are separate binaries in the same repo, each with a focused responsibility.

## Architecture

### Component diagram

```
┌─────────────────────────────────────────────────────────────────────────┐
│  Pod: cli-mcp-server (Deployment, N replicas)                           │
│                                                                         │
│  ┌──────────────────┐     ┌───────────────────────────────────────────┐ │
│  │ kube-rbac-proxy  │     │ cli-mcp-server (main container)           │ │
│  │                  │     │                                           │ │
│  │  TLS :8443 ──────┼────▶│  :8080                                    │ │
│  │  TokenReview     │     │                                           │ │
│  │  Bearer auth     │     │  ┌──────────┐  ┌───────────────────────┐  │ │
│  │                  │     │  │ MCP SDK  │  │ Session Manager       │  │ │
│  │                  │     │  │ Server   │  │                       │  │ │
│  └──────────────────┘     │  │          │  │ Create pod (client-go)│  │ │
│                           │  │ /mcp     │──▶ Discover by label     │  │ │
│                           │  │ /sessions│  │ Proxy to agent HTTP   │  │ │
│                           │  │ /metrics │  │ Cleanup on DELETE/TTL │  │ │
│                           │  │ /live    │  │                       │  │ │
│                           │  │ /health  │  │                       │  │ │
│                           │  └──────────┘  └───────────────────────┘  │ │
│                           │                        │                  │ │
│                           └────────────────────────┼──────────────────┘ │
│                                                    │                    │
└────────────────────────────────────────────────────┼────────────────────┘
                                                     │ HTTP (pod IP:8090, HMAC bearer token)
                                                     ▼
┌─────────────────────────────────────────────────────────────────────────┐
│  Pod: cli-mcp-sandbox-<session-id> (one per investigation)              │
│                                                                         │
│  ┌─────────────────────────────────────────────────────────────────┐    │
│  │ sandbox-agent (Go binary, HTTP :8090)                           │    │
│  │                                                                 │    │
│  │  POST /exec   → pipe command to persistent bash, return result  │    │
│  │  GET  /health → readiness check                                 │    │
│  │                                                                 │    │
│  │  Persistent bash process (stdin/stdout/stderr pipes)            │    │
│  └─────────────────────────────────────────────────────────────────┘    │
│                                                                         │
│  CLIs: oc, kubectl          Unix: jq, yq, grep, awk, curl               │
│  /config/kubeconfig (ro)    /workspace/ (emptyDir, writable)            │
│                                                                         │
│  Labels: tarsy.redhat.com/session-id=<id>                               │
│          tarsy.redhat.com/component=cli-mcp-sandbox                     │
│                                                                         │
│  NetworkPolicy: ingress from cli-mcp-server only                        │
│                 egress to K8s API servers only                          │
└─────────────────────────────────────────────────────────────────────────┘
                     │
                     ▼
            Target K8s clusters (via read-only kubeconfig)
```

### Request flow

1. TARSy sends `tools/call` with tool `bash` and params `{"command": "oc get pods -n foo --context=rm1"}` — the `X-Session-ID: inv-abc123` header is automatically attached by TARSy's MCP client
2. kube-rbac-proxy validates the bearer token via TokenReview, forwards to `:8080`
3. MCP SDK dispatches to the registered `bash` handler, which extracts the session ID from `req.Extra.Header.Get("X-Session-ID")`
4. Session manager looks up pod by label `tarsy.redhat.com/session-id=inv-abc123`
   - **Cache hit:** use cached pod IP
   - **Cache miss:** query K8s API for pods with that label. If found, cache and use. If not found, create new sandbox pod, wait for ready, cache pod IP.
5. HTTP POST to `http://<pod-ip>:8090/exec` with `{"command": "oc get pods -n foo --context=rm1", "timeout": 60}` and `Authorization: Bearer <HMAC-token>` (derived from shared secret + session ID)
6. Sandbox agent pipes command to persistent bash, captures stdout/stderr, returns JSON response
7. MCP server returns `mcp.CallToolResult` to TARSy as-is. Output treatment (summarization, masking) is TARSy's responsibility.

## Core Concepts

### Session management

The MCP server runs in **stateless mode** (`Stateless: true` in `StreamableHTTPOptions`), consistent with all other MCP servers (`mcp-server-devsandbox`, `kubernetes-mcp-server`, etc.). This enables multi-replica deployment behind a load balancer with no sticky sessions.

Session identification uses the **`X-Session-ID` HTTP header**, injected automatically by TARSy's MCP client on every request. TARSy sets this to the investigation ID. The LLM never sees or manages session IDs — they are not tool parameters. This eliminates an entire class of LLM errors (hallucinated IDs, typos, inconsistent values across calls) while keeping the MCP server truly stateless — any replica can serve any request by looking up the pod via Kubernetes labels.

On the server side, each tool handler extracts the session ID from the MCP SDK's `RequestExtra.Header` — a first-class SDK capability where `StreamableHTTPHandler` populates the full `http.Header` from each incoming request.

### MCP tool: `bash`

The name `bash` is the most common tool name for shell execution across LLM agent frameworks, making it immediately recognizable. It avoids collision with `exec` (which in K8s/Docker means "attach to a running container").

| Parameter | Type | Required | Description |
|---|---|---|---|
| `command` | string | yes | Shell command to execute (full bash — pipes, redirects, chaining) |
| `timeout` | int | no | Max execution time in seconds (default 60, max 300) |

Session routing is handled transparently via the `X-Session-ID` HTTP header — the LLM never needs to manage session identity.

```go
type BashInput struct {
    Command string `json:"command" jsonschema:"required,description=Shell command to execute (full bash — pipes, redirects, chaining supported)"`
    Timeout *int   `json:"timeout,omitempty" jsonschema:"description=Max execution time in seconds (default 60, max 300)"`
}
```

Example calls:

```
bash(command="oc get clusteroperators --context=rm1")
bash(command="oc get pods -n openshift-ingress --context=rm1 -o json | jq '.items[] | {name: .metadata.name, ready: .status.containerStatuses[0].ready}'")
bash(command="diff <(oc get pods --context=rm1 -o name) <(oc get pods --context=rm2 -o name)")
```

Working directory and environment variables persist between calls within the same session — maintained by the persistent bash process in the sandbox agent.

### Session manager

The component inside `cli-mcp-server` that manages sandbox pod lifecycle. It operates statelessly — all durable state is in Kubernetes labels.

```go
type SessionManager struct {
    clientset  kubernetes.Interface
    namespace  string
    config     SandboxConfig
    cache      *PodCache
    logger     *slog.Logger
}

type SandboxConfig struct {
    Image          string        // sandbox agent container image
    CPURequest     string        // default: "100m"
    CPULimit       string        // default: "500m"
    MemoryRequest  string        // default: "128Mi"
    MemoryLimit    string        // default: "512Mi"
    IdleTimeout    time.Duration // default: 30m
    KubeconfigPath string        // path to kubeconfig secret for sandbox pods
    HMACKey        []byte        // shared secret for deriving per-session agent auth tokens
    WarmPoolSize   int           // number of pre-warmed pods to maintain (0 = disabled)
}
```

**Operations:**

1. **GetOrCreatePod(sessionID)** — looks up pod by label, assigns from warm pool, or creates on demand
   - Label query: `tarsy.redhat.com/session-id=<sessionID>,tarsy.redhat.com/component=cli-mcp-sandbox`
   - **Cache hit:** return cached pod IP
   - **Cache miss, warm pool enabled:** list unassigned pods (`tarsy.redhat.com/component=cli-mcp-sandbox` without `tarsy.redhat.com/session-id`). Claim one by patching labels to add session ID (first writer wins — concurrent replicas retry with a different pod). Create per-session auth Secret. Call `POST /assign` on the agent to deliver the HMAC token. Trigger async pool replenishment.
   - **Cache miss, no warm pod available (or pool disabled):** create auth Secret, create sandbox pod with `secretKeyRef`, wait for `PodRunning` + agent `/health` 200.
   - Returns pod IP for agent communication
   - Caches result in `PodCache` (TTL ~30s) to avoid per-request K8s API calls

2. **ExecuteCommand(sessionID, command, timeout)** — resolves pod, sends HTTP POST to agent
   - Calls `GetOrCreatePod` for the pod IP
   - Computes `HMAC-SHA256(HMACKey, sessionID)` and includes it as `Authorization: Bearer <token>` in the HTTP request
   - HTTP POST to `http://<pod-ip>:8090/exec`
   - Updates `tarsy.redhat.com/last-activity` annotation on the pod (patch via `client-go`)
   - Returns structured response as-is (stdout, stderr, exit code, duration)

3. **CleanupSession(sessionID)** — deletes the sandbox pod and its auth Secret
   - Called when session ends (via `DELETE /sessions/{id}` or TTL expiry)
   - Deletes pod by label selector, then deletes the `cli-mcp-sandbox-auth-<sessionID>` Secret

4. **CleanupStale()** — periodic goroutine that deletes pods past TTL
   - Runs every 5 minutes
   - Lists all sandbox pods, checks `tarsy.redhat.com/last-activity` annotation
   - Deletes pods idle beyond `IdleTimeout`

5. **ReconcilePool()** — periodic goroutine that maintains the warm pod pool (only when `WarmPoolSize > 0`)
   - Runs every 30 seconds
   - Lists unassigned sandbox pods (label `tarsy.redhat.com/component=cli-mcp-sandbox` without `tarsy.redhat.com/session-id`)
   - Creates new pods to reach the target pool size. Warm pods are created without a session ID label and without an auth Secret — the agent starts in "unassigned" mode.
   - Also triggered asynchronously after each warm pod assignment

**Pod cache:**

```go
type PodCache struct {
    mu      sync.RWMutex
    entries map[string]*PodCacheEntry
    ttl     time.Duration // ~30s
}

type PodCacheEntry struct {
    podName   string
    podIP     string
    cachedAt  time.Time
}
```

On MCP server restart, the cache is empty. The first request for each active session triggers a label query, which rediscovers the existing pod and repopulates the cache. No session data is lost because the sandbox pod (and its persistent bash process) is still running.

### Sandbox agent

The lightweight Go binary running inside each sandbox pod. ~200-300 lines of Go.

**Authentication:** The agent requires a bearer token on every `/exec` request. The token can be delivered in two ways depending on how the pod was created:

- **On-demand pods:** Token injected via `SANDBOX_AUTH_TOKEN` env var (from per-session Kubernetes Secret via `secretKeyRef`). The agent reads it at startup.
- **Warm pool pods:** Token delivered at assignment time via `POST /assign`. The agent starts without a token and rejects `/exec` calls until assigned.

In both cases, token comparison uses `hmac.Equal()` (constant-time) to prevent timing side-channel attacks. The `/health` endpoint is unauthenticated (required for K8s readiness probes).

**Token delivery for on-demand pods:** The session manager creates a Secret (`cli-mcp-sandbox-auth-<sessionID>`) containing the HMAC-derived token before creating the sandbox pod. The pod references it via `env[].valueFrom.secretKeyRef`. This keeps the token out of the pod spec — reading it requires `get secrets` permission, which is more restricted than `get pods`. The Secret is deleted alongside the pod during session cleanup.

**Token delivery for warm pool pods:** Warm pods are created without a Secret or token. When assigned to a session, the session manager calls `POST /assign` on the agent, passing the HMAC token in the request body. The agent stores it in memory and transitions to "assigned" mode. The `/assign` endpoint can only be called once — subsequent calls return 409 Conflict.

**API:**

| Endpoint | Method | Auth | Description |
|---|---|---|---|
| `/exec` | POST | Bearer token required | Execute a command in the persistent bash session |
| `/assign` | POST | None (callable once) | Deliver auth token and transition to assigned mode. Returns 409 if already assigned. |
| `/health` | GET | None | Readiness check (returns 200 if agent is running) |

**Request/response:**

```go
type ExecRequest struct {
    Command string `json:"command"`
    Timeout int    `json:"timeout,omitempty"` // seconds, default 60
}

type ExecResponse struct {
    Stdout     string `json:"stdout"`
    Stderr     string `json:"stderr"`
    ExitCode   int    `json:"exit_code"`
    DurationMs int64  `json:"duration_ms"`
}

type AssignRequest struct {
    Token string `json:"token"` // HMAC-derived bearer token for this session
}
```

**Agent state machine:** The agent has two states: `unassigned` (initial state for warm pool pods) and `assigned` (after receiving token via env var or `/assign`). In `unassigned` state, `/exec` returns 503; `/assign` accepts the token and transitions to `assigned`. In `assigned` state, `/exec` requires the bearer token; `/assign` returns 409.

**Persistent bash session (pipe-based with delimiter protocol):**

The agent manages a persistent `bash --norc --noprofile` process using Go's `os/exec` with `StdinPipe()`, `StdoutPipe()`, and `StderrPipe()`. No external dependencies — pure Go stdlib. This approach gives clean stdout/stderr separation (important for structured responses) and avoids PTY complexity that adds no value for LLM consumption.

```go
type BashSession struct {
    cmd    *exec.Cmd
    stdin  io.WriteCloser
    stdout *bufio.Reader
    stderr *bufio.Reader
    mu     sync.Mutex
}

func NewBashSession() (*BashSession, error) {
    cmd := exec.Command("bash", "--norc", "--noprofile")
    cmd.Env = append(os.Environ(), "PS1=", "PS2=")
    stdin, _ := cmd.StdinPipe()
    stdout, _ := cmd.StdoutPipe()
    stderr, _ := cmd.StderrPipe()
    if err := cmd.Start(); err != nil {
        return nil, err
    }
    return &BashSession{
        cmd:    cmd,
        stdin:  stdin,
        stdout: bufio.NewReader(stdout),
        stderr: bufio.NewReader(stderr),
    }, nil
}
```

**Delimiter protocol:** Each command execution wraps the user's command with unique delimiters to isolate its output from the continuous stream. A UUID-based delimiter per request avoids collisions with command output.

```go
func (b *BashSession) Execute(command string, timeout time.Duration) (*ExecResponse, error) {
    b.mu.Lock()
    defer b.mu.Unlock()

    delimiter := fmt.Sprintf("__SANDBOX_%s__", uuid.NewString()[:8])

    // Write wrapped command to stdin:
    //   echo <delimiter>_START
    //   <command>
    //   exitcode=$?
    //   echo <delimiter>_END
    //   echo <delimiter>_EXIT_${exitcode} >&2
    //   echo ${exitcode}  (for exit code capture)
    wrapped := fmt.Sprintf(
        "echo %[1]s_START\n%[2]s\n__exit=$?\necho %[1]s_END\necho %[1]s_EXIT_${__exit} >&2\n",
        delimiter, command,
    )
    // ... write to stdin, read stdout until END delimiter,
    // read stderr until EXIT delimiter, parse exit code ...
}
```

The reader goroutines scan for the delimiter markers in stdout and stderr. Stdout content between `_START` and `_END` is the command output. The exit code is parsed from the stderr `_EXIT_<code>` marker.

**Timeout handling:** The agent wraps command execution with a `time.After` timer. If the timeout fires, it sends `SIGKILL` to the bash process group for the running command (not the bash process itself — bash survives and can run the next command).

**Crash recovery:** If the bash process dies (OOM, signal), the agent detects this when the pipe read returns `io.EOF` or the process `Wait()` returns. On the next `/exec` request, it respawns a new bash process and returns the command result with `stderr` containing `"[session state was reset — previous bash process exited]"`. The `/health` endpoint returns 503 if bash is dead and not yet respawned.

**The agent is simple by design.** It doesn't implement security filtering or session routing — those concerns belong in the MCP server or the sandbox boundary.

### Session cleanup

Two mechanisms work together:

**1. Explicit `DELETE /sessions/{id}` endpoint (primary):**

A plain HTTP endpoint (not an MCP tool) for session lifecycle management. TARSy calls this when the investigation chain completes — the LLM is not involved in session cleanup. This separation is intentional: MCP tools are for investigation (the LLM uses them), lifecycle management is TARSy's job.

```go
func (h *SessionDeleteHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    sessionID := r.PathValue("id")
    if sessionID == "" {
        http.Error(w, "missing session ID", http.StatusBadRequest)
        return
    }
    if err := h.sessions.CleanupSession(r.Context(), sessionID); err != nil {
        http.Error(w, fmt.Sprintf("failed to end session: %s", err), http.StatusInternalServerError)
        return
    }
    w.WriteHeader(http.StatusNoContent)
}
```

TARSy calls this after the investigation chain finishes, using the same session/investigation ID it injected as `X-Session-ID`.

**2. TTL safety net (fallback):**

A periodic cleanup goroutine (runs every 5 minutes) lists all sandbox pods and deletes any that have been idle beyond `--idle-timeout` (default 30 minutes). Idle time is determined by the `tarsy.redhat.com/last-activity` annotation, which the session manager updates on every `bash` call.

This catches edge cases: TARSy crash, network failure on DELETE call, or any situation where explicit cleanup doesn't happen.

```go
func (m *SessionManager) startCleanupLoop(ctx context.Context) {
    ticker := time.NewTicker(5 * time.Minute)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            m.cleanupStale(ctx)
        }
    }
}

func (m *SessionManager) cleanupStale(ctx context.Context) {
    pods, err := m.clientset.CoreV1().Pods(m.namespace).List(ctx, metav1.ListOptions{
        LabelSelector: "tarsy.redhat.com/component=cli-mcp-sandbox",
    })
    if err != nil {
        m.logger.Error("failed to list sandbox pods for cleanup", "error", err)
        return
    }
    for _, pod := range pods.Items {
        lastActivity, _ := time.Parse(time.RFC3339, pod.Annotations["tarsy.redhat.com/last-activity"])
        if time.Since(lastActivity) > m.config.IdleTimeout {
            sessionID := pod.Labels["tarsy.redhat.com/session-id"]
            m.logger.Info("cleaning up stale sandbox pod", "session_id", sessionID, "idle", time.Since(lastActivity))
            _ = m.clientset.CoreV1().Pods(m.namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
            _ = m.clientset.CoreV1().Secrets(m.namespace).Delete(ctx, fmt.Sprintf("cli-mcp-sandbox-auth-%s", sessionID), metav1.DeleteOptions{})
            m.cache.Remove(sessionID)
        }
    }
}
```

### Sandbox pod spec

The pod spec is built in Go code with CLI flags for the variable parts (`--sandbox-image`, `--idle-timeout`, resource limits via `SandboxConfig`). This is consistent with how `mcp-server-devsandbox` handles configuration — CLI flags via Cobra, no external templates. If operators later need to customize tolerations or node selectors, those can be added as additional flags.

Each sandbox pod is created by the session manager with the following spec:

```go
func (m *SessionManager) buildPodSpec(sessionID string) *corev1.Pod {
    return &corev1.Pod{
        ObjectMeta: metav1.ObjectMeta{
            GenerateName: "cli-mcp-sandbox-",
            Namespace:    m.namespace,
            Labels: map[string]string{
                "tarsy.redhat.com/session-id": sessionID,
                "tarsy.redhat.com/component":  "cli-mcp-sandbox",
            },
            Annotations: map[string]string{
                "tarsy.redhat.com/created-at":    time.Now().UTC().Format(time.RFC3339),
                "tarsy.redhat.com/last-activity": time.Now().UTC().Format(time.RFC3339),
            },
        },
        Spec: corev1.PodSpec{
            ServiceAccountName: "cli-mcp-investigation-sa",
            RestartPolicy:      corev1.RestartPolicyOnFailure,
            SecurityContext: &corev1.PodSecurityContext{
                RunAsNonRoot: ptr(true),
                RunAsUser:    ptr(int64(1001)),
                RunAsGroup:   ptr(int64(1001)),
            },
            Containers: []corev1.Container{{
                Name:  "sandbox-agent",
                Image: m.config.Image,
                Ports: []corev1.ContainerPort{{
                    ContainerPort: 8090,
                    Protocol:      corev1.ProtocolTCP,
                }},
                Resources: corev1.ResourceRequirements{
                    Requests: corev1.ResourceList{
                        corev1.ResourceCPU:    resource.MustParse(m.config.CPURequest),
                        corev1.ResourceMemory: resource.MustParse(m.config.MemoryRequest),
                    },
                    Limits: corev1.ResourceList{
                        corev1.ResourceCPU:    resource.MustParse(m.config.CPULimit),
                        corev1.ResourceMemory: resource.MustParse(m.config.MemoryLimit),
                    },
                },
                SecurityContext: &corev1.SecurityContext{
                    AllowPrivilegeEscalation: ptr(false),
                    Capabilities: &corev1.Capabilities{
                        Drop: []corev1.Capability{"ALL"},
                    },
                },
                VolumeMounts: []corev1.VolumeMount{
                    {Name: "kubeconfig", MountPath: "/config", ReadOnly: true},
                    {Name: "workspace", MountPath: "/workspace"},
                },
                ReadinessProbe: &corev1.Probe{
                    ProbeHandler: corev1.ProbeHandler{
                        HTTPGet: &corev1.HTTPGetAction{
                            Path: "/health",
                            Port: intstr.FromInt(8090),
                        },
                    },
                    InitialDelaySeconds: 2,
                    PeriodSeconds:       10,
                },
                Env: []corev1.EnvVar{
                    {Name: "KUBECONFIG", Value: "/config/kubeconfig"},
                    {Name: "HOME", Value: "/workspace"},
                    {Name: "SANDBOX_AUTH_TOKEN", ValueFrom: &corev1.EnvVarSource{
                        SecretKeyRef: &corev1.SecretKeySelector{
                            LocalObjectReference: corev1.LocalObjectReference{
                                Name: fmt.Sprintf("cli-mcp-sandbox-auth-%s", sessionID),
                            },
                            Key: "token",
                        },
                    }},
                },
            }},
            Volumes: []corev1.Volume{
                {
                    Name: "kubeconfig",
                    VolumeSource: corev1.VolumeSource{
                        Secret: &corev1.SecretVolumeSource{
                            SecretName: "cli-mcp-investigation-kubeconfig",
                        },
                    },
                },
                {
                    Name: "workspace",
                    VolumeSource: corev1.VolumeSource{
                        EmptyDir: &corev1.EmptyDirVolumeSource{},
                    },
                },
            },
        },
    }
}
```

**Pod labels and annotations:**

| Label/Annotation | Purpose |
|---|---|
| `tarsy.redhat.com/session-id` (label) | Session→pod routing. Used for label-based lookup. |
| `tarsy.redhat.com/component` (label) | Identifies sandbox pods for bulk operations (list, cleanup). |
| `tarsy.redhat.com/created-at` (annotation) | Creation timestamp for debugging. |
| `tarsy.redhat.com/last-activity` (annotation) | Updated on each command execution. Used by TTL cleanup. |

### Output handling

The MCP server returns command output **as-is** — no truncation, no transformation. Output treatment (data masking, summarization, token compression) is TARSy's responsibility, handled by its existing `data_masking` and `summarization` configs per MCP server.

### Tool handler

The `bash` handler ties together session extraction, session management, and command proxying:

```go
func (h *BashHandler) Handle(ctx context.Context, req *mcp.ServerRequest[mcp.CallToolParams]) (*mcp.CallToolResult, error) {
    sessionID := req.Extra.Header.Get("X-Session-ID")
    if sessionID == "" {
        return nil, fmt.Errorf("missing X-Session-ID header")
    }

    var input BashInput
    if err := json.Unmarshal(req.Params.Arguments, &input); err != nil {
        return nil, fmt.Errorf("invalid arguments: %w", err)
    }

    timeout := 60
    if input.Timeout != nil && *input.Timeout > 0 {
        timeout = min(*input.Timeout, 300)
    }

    result, err := h.sessions.ExecuteCommand(ctx, sessionID, input.Command, timeout)
    if err != nil {
        return nil, fmt.Errorf("sandbox exec failed: %w", err)
    }

    output := BashOutput{
        Stdout:     result.Stdout,
        Stderr:     result.Stderr,
        ExitCode:   result.ExitCode,
        DurationMs: result.DurationMs,
    }

    jsonBytes, _ := json.Marshal(output)

    return &mcp.CallToolResult{
        Content: []mcp.Content{
            mcp.TextContent{Text: string(jsonBytes)},
        },
        IsError: result.ExitCode != 0,
    }, nil
}
```

**Non-zero exit codes are returned as tool results with `IsError: true`, not as MCP errors.** A command returning exit code 1 (e.g., `oc get pod nonexistent` → "NotFound") is still useful output for the LLM. Only infrastructure failures (pod creation failed, agent unreachable) return MCP errors.

### Error handling

| Scenario | Behavior |
|---|---|
| **Pod creation fails** | Return MCP error. Session manager retries once. |
| **Agent unreachable** (pod IP, network) | Return MCP error with "sandbox agent unreachable" message. Session manager invalidates cache entry. |
| **Bash process died** (OOM, signal) | Agent detects on next `/exec` (pipe EOF), respawns a new bash process, returns result with `stderr` containing "session state was reset". The agent process itself stays alive — only the bash child process is restarted. |
| **Agent process crashed** | K8s `RestartPolicy: OnFailure` restarts the pod container. The pod gets a new IP. The MCP server's cache entry becomes stale; the next request triggers a cache miss, rediscovers the pod by label, and caches the new IP. Session state (env vars, files) is lost. |
| **Command timeout** | Agent kills the command after timeout, returns partial output + exit code 137 (SIGKILL). |
| **Pod evicted** (node pressure) | Next request creates a new pod. Session state is lost — the LLM sees "new session" in the response. |
| **K8s API unreachable** | Session manager returns MCP error. Health check fails, pod marked not ready. |

### Server bootstrap

```go
func main() {
    var (
        address        string
        transport      string
        stateless      bool
        namespace      string
        sandboxImage   string
        kubeconfigPath string
        hmacKeyPath    string
        idleTimeout    time.Duration
        warmPoolSize   int
    )

    rootCmd := &cobra.Command{
        Use:   "cli-mcp-server",
        Short: "Sandboxed exec environment MCP server for LLM investigation",
        RunE: func(cmd *cobra.Command, args []string) error {
            return runServer(runConfig{
                address:        address,
                transport:      transport,
                stateless:      stateless,
                namespace:      namespace,
                sandboxImage:   sandboxImage,
                kubeconfigPath: kubeconfigPath,
                hmacKeyPath:    hmacKeyPath,
                idleTimeout:    idleTimeout,
                warmPoolSize:   warmPoolSize,
            })
        },
    }

    rootCmd.Flags().StringVarP(&address, "address", "a", "localhost:8080", "Server address (host:port)")
    rootCmd.Flags().StringVarP(&transport, "transport", "t", "stdio", "Transport (stdio, http)")
    rootCmd.Flags().BoolVar(&stateless, "stateless", false, "Enable stateless mode for multi-replica (required for HTTP)")
    rootCmd.Flags().StringVar(&namespace, "namespace", "tarsy", "Namespace for sandbox pods")
    rootCmd.Flags().StringVar(&sandboxImage, "sandbox-image", "", "Container image for sandbox pods (required)")
    rootCmd.Flags().StringVar(&kubeconfigPath, "kubeconfig", "", "Path to combined kubeconfig for sandbox pods")
    rootCmd.Flags().StringVar(&hmacKeyPath, "hmac-key-file", "", "Path to file containing HMAC shared secret for sandbox agent auth")
    rootCmd.Flags().DurationVar(&idleTimeout, "idle-timeout", 30*time.Minute, "Idle timeout for sandbox pods")
    rootCmd.Flags().IntVar(&warmPoolSize, "warm-pool-size", 0, "Number of pre-warmed sandbox pods to maintain (0 = disabled, create on demand)")

    if err := rootCmd.Execute(); err != nil {
        log.Fatalf("Error: %v", err)
    }
}
```

`runServer`:
1. Creates Kubernetes clientset (in-cluster config — the MCP server runs in K8s)
2. Creates `SessionManager` with sandbox config
3. Creates `mcp.Server` with `mcp-common` middleware (metrics, logging)
4. Registers `bash` tool
5. Starts stale pod cleanup goroutine
6. Starts warm pool reconciliation goroutine (if `WarmPoolSize > 0`)
7. Sets up HTTP mux with `/mcp`, `/sessions/{id}`, `/metrics`, `/live`, `/health`
7. Signal handling for graceful shutdown

**Health check:** The `/health` endpoint verifies the session manager can reach the Kubernetes API (a lightweight namespace get). It does not check individual sandbox pods — those are checked lazily on request.

## Go Types Summary

```go
// --- MCP server types ---

type BashInput struct {
    Command string `json:"command" jsonschema:"required,description=Shell command to execute"`
    Timeout *int   `json:"timeout,omitempty" jsonschema:"description=Max execution time in seconds (default 60)"`
}

// Session ID is extracted from the X-Session-ID HTTP header, not from tool params.

type BashOutput struct {
    Stdout     string `json:"stdout"`
    Stderr     string `json:"stderr"`
    ExitCode   int    `json:"exit_code"`
    DurationMs int64  `json:"duration_ms"`
}

// session_end is not an MCP tool — it's a plain HTTP DELETE /sessions/{id} endpoint.

// --- Sandbox agent types (shared contract) ---

type ExecRequest struct {
    Command string `json:"command"`
    Timeout int    `json:"timeout,omitempty"`
}

type ExecResponse struct {
    Stdout     string `json:"stdout"`
    Stderr     string `json:"stderr"`
    ExitCode   int    `json:"exit_code"`
    DurationMs int64  `json:"duration_ms"`
}

type AssignRequest struct {
    Token string `json:"token"`
}
```

## Project Structure

```
cli-mcp-server/
├── cmd/
│   ├── server/
│   │   └── main.go                # MCP server entry point (Cobra, flags, bootstrap)
│   └── agent/
│       └── main.go                # Sandbox agent entry point (HTTP server, bash session)
├── pkg/
│   ├── session/
│   │   ├── manager.go             # SessionManager — pod lifecycle, cache, cleanup
│   │   ├── manager_test.go
│   │   ├── pool.go                # Warm pod pool — reconciliation, claiming, replenishment
│   │   ├── pool_test.go
│   │   ├── cache.go               # PodCache — in-memory TTL cache for pod IPs
│   │   └── cache_test.go
│   ├── agent/
│   │   ├── client.go              # HTTP client for sandbox agent API
│   │   ├── client_test.go
│   │   └── types.go               # ExecRequest/ExecResponse (shared contract)
│   ├── sandbox/
│   │   ├── bash.go                # Persistent bash session management
│   │   ├── bash_test.go
│   │   ├── handler.go             # HTTP handlers (POST /exec, POST /assign, GET /health)
│   │   └── handler_test.go
│   ├── server/
│   │   ├── server.go              # MCP server setup, middleware, endpoints
│   │   └── server_test.go
│   └── tools/
│       ├── bash.go                # bash tool — handler, registration
│       └── bash_test.go
├── docs/
│   └── proposals/
│       ├── cli-mcp-server-sketch.md
│       ├── cli-mcp-server-proposal.md
│       └── cli-mcp-server-design.md
├── Containerfile.server            # MCP server image (multi-stage Go build)
├── Containerfile.agent             # Sandbox agent image (agent binary + CLIs + utils)
├── Makefile
├── go.mod
├── go.sum
└── README.md
```

Two binaries, two images, one repo. The `pkg/agent/types.go` file contains the shared request/response contract between the server (as HTTP client) and the agent (as HTTP server).

## Container images

### MCP server (`Containerfile.server`)

```dockerfile
FROM mirror.gcr.io/library/golang:1.25-alpine AS go-builder
RUN apk add --no-cache git make
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags="-X main.version=${VERSION}" -o bin/cli-mcp-server ./cmd/server/

FROM gcr.io/distroless/static-debian12
COPY --from=go-builder /workspace/bin/cli-mcp-server /usr/bin/
USER 1001
ENTRYPOINT ["/usr/bin/cli-mcp-server"]
CMD ["--transport", "http", "--stateless"]
```

The server image is minimal — no CLIs needed. It only manages pods and proxies HTTP.

### Sandbox agent (`Containerfile.agent`)

```dockerfile
FROM mirror.gcr.io/library/golang:1.25-alpine AS go-builder
RUN apk add --no-cache git make
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags="-X main.version=${VERSION}" -o bin/sandbox-agent ./cmd/agent/

FROM quay.io/codeready-toolchain/oc-client-base-minimal

# Agent binary
COPY --from=go-builder /workspace/bin/sandbox-agent /usr/bin/

# Unix utilities for investigation
RUN microdnf install -y jq yq curl && microdnf clean all

# Non-root user
RUN groupadd -g 1001 sandbox \
    && useradd -u 1001 -g 1001 -r -s /bin/bash -d /workspace sandbox \
    && mkdir -p /workspace && chown -R 1001:1001 /workspace

USER 1001
WORKDIR /workspace
ENTRYPOINT ["/usr/bin/sandbox-agent"]
```

The agent image includes CLIs and utilities. Adding a future CLI means a `RUN` layer — no code changes. Images are built with `podman build`.

## Kubernetes Deployment (sandbox-sre)

Manifests live in `sandbox-sre/components/cli-mcp-server/`.

### Key manifests

| File | Purpose |
|---|---|
| `deployment.yaml` | MCP server: kube-rbac-proxy sidecar + main container |
| `service.yaml` | ClusterIP port 8443, serving cert annotation |
| `service-accounts.yaml` | `cli-mcp-server` SA (pod management RBAC) |
| `investigation-sa.yaml` | `cli-mcp-investigation-sa` SA (read-only cluster access for sandbox pods) |
| `network-policy.yaml` | Ingress to MCP server + ingress/egress for sandbox pods |
| `kustomization.yaml` | Ties everything together |

### Deployment args

```yaml
args:
  - --transport
  - http
  - --address
  - 127.0.0.1:8080
  - --stateless
  - --namespace
  - tarsy
  - --sandbox-image
  - quay.io/codeready-toolchain/cli-mcp-sandbox:latest
  - --kubeconfig
  - /config/kubeconfig
  - --hmac-key-file
  - /secrets/hmac/key
  - --idle-timeout
  - 30m
  - --warm-pool-size
  - "0"
```

### RBAC

**`cli-mcp-server` SA** (MCP server pod):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: cli-mcp-server
  namespace: tarsy
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["create", "delete", "get", "list", "watch", "patch"]
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["create", "delete"]
```

Scoped to `tarsy` namespace only. `patch` on pods is needed to update the `last-activity` annotation. `create`/`delete` on secrets is needed for per-session auth token delivery.

**`cli-mcp-investigation-sa` SA** (mounted into sandbox pods):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: cli-mcp-investigation-readonly
subjects:
  - kind: ServiceAccount
    name: cli-mcp-investigation-sa
    namespace: tarsy
roleRef:
  kind: ClusterRole
  name: view   # standard K8s read-only
```

Plus custom ClusterRoles for `list-nodes` and `kube-investigation-readonly` (cluster-scoped reads). No `pods/exec`, no write operations.

### NetworkPolicy for sandbox pods

Defense-in-depth alongside HMAC-based agent authentication. Even if the token is somehow compromised, NetworkPolicy limits who can reach the agent.

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: cli-mcp-sandbox
  namespace: tarsy
spec:
  podSelector:
    matchLabels:
      tarsy.redhat.com/component: cli-mcp-sandbox
  policyTypes:
    - Ingress
    - Egress
  ingress:
    - from:
        - podSelector:
            matchLabels:
              app: cli-mcp-server
      ports:
        - port: 8090
          protocol: TCP
  egress:
    - to:
        - ipBlock:
            cidr: <k8s-api-server-cidr>
      ports:
        - port: 6443
          protocol: TCP
    - to:  # DNS resolution
        - namespaceSelector: {}
          podSelector:
            matchLabels:
              k8s-app: kube-dns
      ports:
        - port: 53
          protocol: UDP
        - port: 53
          protocol: TCP
```

## TARSy Integration

### tarsy.yaml configuration

```yaml
mcp_servers:
  cli-mcp-server:
    transport:
      type: "http"
      url: "https://cli-mcp-server:8443/mcp"
      bearer_token: "{{.CLI_MCP_BEARER_TOKEN}}"
      timeout: 90
      verify_ssl: false
      custom_headers:
        X-Session-ID: "{{.SESSION_ID}}"  # Injected per-session by TARSy (investigation ID)
    instructions: |
      This server provides a sandboxed shell environment for cluster investigation.
      You have full bash access: pipes, redirects, chaining, and standard Unix tools (jq, grep, awk).
      Available CLIs: oc, kubectl.
      Available clusters: rm1, rm2, rm3. Use --context=<cluster> to target a specific cluster.
      Your workspace at /workspace is ephemeral — use it for intermediate files during investigation.
      All cluster access is read-only via RBAC.
    data_masking:
      enabled: true
      pattern_groups: ["kubernetes", "security"]
      patterns: ["certificate", "token", "email"]
    summarization:
      enabled: true
      summary_max_token_limit: 1200
```

### Required TARSy changes

This MCP server depends on two TARSy capabilities that don't exist yet:

**1. `custom_headers` in `TransportConfig`**

TARSy's `config.TransportConfig` struct (in `pkg/config/types.go`) needs a new field:

```go
type TransportConfig struct {
    // ... existing fields ...
    CustomHeaders map[string]string `yaml:"custom_headers,omitempty"` // Per-request custom headers
}
```

Headers with template variables (like `{{.SESSION_ID}}`) must be resolved **per-session**, not at config load time. This requires:
- `mcp.ClientFactory` to accept per-session template variables (e.g., session/investigation ID)
- `mcp.createHTTPTransport` to build an HTTP client with a round-tripper that injects the resolved custom headers — the same pattern already used by `bearerTokenTransport` in `pkg/mcp/transport.go`

**2. Session cleanup HTTP call**

After an investigation chain completes, TARSy should call `DELETE https://<mcp-server>:8443/sessions/{id}` to trigger immediate sandbox pod cleanup. This requires:
- A helper in the session executor that constructs a plain HTTP client from the MCP server's transport config (URL base, bearer token, TLS settings)
- A call site in `pkg/queue/executor.go` after the chain finishes, using the same investigation ID passed as `X-Session-ID`

As a fallback, the TTL safety net on the MCP server handles cleanup even if TARSy doesn't call DELETE — so this integration can be deferred to a follow-up if needed.

## Testing Strategy

### Unit tests

| Package | What's tested | Approach |
|---|---|---|
| `pkg/session` | Pod creation, warm pool claiming, cache hit/miss, cleanup, stale detection, pool reconciliation | `client-go` fake clientset (`fake.NewSimpleClientset`) |
| `pkg/agent` (client) | HTTP client for agent API — success, timeout, error, assign | `httptest.NewServer` with canned responses |
| `pkg/sandbox` | Bash session — command execution, exit codes, env persistence, crash recovery, assign state machine | Real `bash` process in test (short-lived) |
| `pkg/tools` | `bash` handler — header extraction, session routing, error mapping | Mock `SessionManager` interface |
| `pkg/server` | Tool registration, health check, middleware | In-memory server |

### Integration tests

Unit tests with fakes cover the critical paths. Integration tests with real sandbox pods can be added later for end-to-end validation if needed — consistent with `mcp-server-devsandbox`'s approach.

## Implementation Plan

### Phase 1: Sandbox agent

1. Implement `pkg/sandbox/bash.go` — persistent bash session (spawn, pipe, command execution, crash detection)
2. Implement `pkg/sandbox/handler.go` — HTTP handlers (`POST /exec`, `GET /health`)
3. Implement `cmd/agent/main.go` — entry point, HTTP server setup
4. Write `Containerfile.agent`
5. Unit tests for bash session management

### Phase 2: MCP server core

1. Initialize Go module with `go-sdk`, `mcp-common`, `client-go`, Cobra
2. Implement `pkg/session/cache.go` — in-memory pod IP cache with TTL
3. Implement `pkg/session/manager.go` — pod lifecycle (create, discover, cleanup, stale)
4. Implement `pkg/agent/client.go` — HTTP client for agent API
5. Implement `pkg/tools/bash.go` — tool handler, registration
6. Implement `cmd/server/main.go` — Cobra root, server bootstrap
7. Implement `pkg/server/server.go` — MCP server setup with middleware
8. Write `Containerfile.server`
9. Unit tests for all packages

### Phase 3: Deployment

1. Create `sandbox-sre/components/cli-mcp-server/` kustomize manifests
2. ServiceAccounts and RBAC (both `cli-mcp-server` and `cli-mcp-investigation-sa`)
3. Service with serving cert annotation
4. NetworkPolicy for both MCP server and sandbox pods
5. Staging overlay with kubeconfig secret reference
6. Add `cli-mcp-server` entry to `tarsy.yaml`

### Phase 4: Production rollout

1. Production overlay
2. Monitor pod lifecycle, command patterns, resource usage
3. Iterate on resource limits and idle timeouts based on real usage
4. Evaluate adding more CLIs (`virtctl`, `helm`) based on agent needs

## Design Decisions Summary

| # | Topic | Decision | Rationale |
|---|---|---|---|
| Q1 | Session identification | `X-Session-ID` HTTP header (injected by TARSy) | Server stays stateless. LLM never manages session IDs — eliminates hallucination/typo errors. Header travels with every request, works with any replica. MCP SDK's `RequestExtra.Header` provides first-class access. |
| Q2 | Session cleanup | `DELETE /sessions/{id}` endpoint (called by TARSy) + TTL safety net (fallback, 30m idle) | Lifecycle management is TARSy's job, not the LLM's. Plain HTTP endpoint keeps MCP tools focused on investigation. TTL catches edge cases. |
| Q3 | Pod template source | Hardcoded Go struct + CLI flags for overrides | Simplest approach, consistent with `mcp-server-devsandbox`. Variable parts (image, resources, namespace, timeout) exposed as flags. Can evolve to ConfigMap overrides later if needed. |
| Q4 | Sandbox agent bash implementation | Pipe-based with delimiter protocol (stdin/stdout/stderr pipes, UUID delimiters) | Clean stdout/stderr separation for structured responses. No external dependencies. Well-known pattern (OpenHands, Claude Code). UUID delimiters prevent collisions. PTY adds complexity with no benefit for LLM consumption. |
| Q5 | Tool naming | `bash` (not `shell_exec` or `exec`) | Most common shell tool name across LLM agent frameworks — maximally familiar to LLMs. Avoids collision with `exec` (which in K8s/Docker means "attach to a running container"). |
| Q6 | Output handling | Return as-is, no truncation | MCP server is a dumb proxy. Output treatment (masking, summarization) is TARSy's responsibility via existing per-server config. |
| Q7 | Sandbox agent auth | HMAC-derived bearer token per session | Defense-in-depth: even if NetworkPolicy is bypassed, agent rejects unauthenticated requests. HMAC is deterministic — any replica computes the same token from the shared secret + session ID. No token storage or lookup needed. |
| Q8 | Warm pod pool | Configurable pool size (`--warm-pool-size`, default 0) | Eliminates cold start latency when enabled. Pool size 0 = create on demand (simplest default). Warm pods have no session ID or token until assigned. Replicas coordinate via label-based claiming (first writer wins). |

**Note on future write capabilities:** If the server later adds write-capable CLIs (e.g., `helm install`, `oc apply`), per-cluster sandbox pods should be considered. With write access, targeting the wrong cluster has destructive consequences, and per-cluster isolation eliminates that risk. For read-only investigation, a shared kubeconfig with all contexts is safe.
