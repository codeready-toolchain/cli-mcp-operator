# CLI MCP — credential-isolating proxy — design questions

**Status:** Decisions recorded
**Related:** [Design document](credential-proxy-design.md) · [Operator HOW](cli-mcp-operator-design.md) · [Operator questions](cli-mcp-operator-questions.md)

Each question has options with trade-offs and a recommendation. Decisions for Q1–Q15 are recorded below. The matching HOW is [credential-proxy-design.md](credential-proxy-design.md) (Final).

Q1–Q15 are decided. This is **Phase 7**: the operator is implemented (children, Ready, two Pod writers, HMAC generate-once, `cli-mcp.redhat.com` labels, dedicated sandbox SA). First-party internal deploy is one catalog consumer; do not bake that environment into the API.

---

## Q1: Who owns the proxy stack vs derived objects?

The umbrella analysis said the MCP server would create the proxy Deployment, route ConfigMap, CA, dummy kubeconfig, and NetworkPolicies. That is a mini-operator inside a process that must stay **stateless and multi-replica** for `bash`. Traditional operators are leader-elected singletons. Dummy kubeconfig and routes **must** be derived from the live investigation Secret + proxy CA (not frozen in git). Child objects also need watches, ownerRefs, and status — which a 30s ensure loop in `cmd/server` would re-invent.

### Option D: Real Kubernetes operator (CLI MCP Operator)

A CRD instance (one CR = one MCP instance / class) is the API. A leader-elected operator Deployment watches it and bootstraps instance infrastructure: MCP server Deployment, proxy Deployment+ClusterIP Service, CA, route ConfigMap, dummy kubeconfig, NetworkPolicies, sandbox SA, and related RBAC wiring.

The MCP server stays a horizontally scaled data plane: `bash`, per-session pods, HMAC claim/create/`exec`. It does **not** reconcile the proxy stack, the MCP Deployment, warm-pool size, or idle GC (those are the operator — see operator Q5). Sessions are not CRs.

GitOps installs the operator once and applies `CliMcpInstance` objects. It does not hand-maintain the proxy Deployment.

- **Pro:** Proper watches (deleted/edited proxy comes back), ownerRefs/GC, status (`Ready` folds proxy children), validation on spec. A second class later is another CR, not a snowflake Deployment. Matches claw-operator’s split (operator owns children; workload process is not the controller). Resolves singleton-vs-stateless: operator is the singleton; MCP replicas stay stateless.
- **Con:** CRD, manager, envtest, operator RBAC — already paid in operator phases 1–5.

**Decision:** Option D — real operator. This proxy design keeps the security/topology WHAT; operator design is HOW. Do not put an ensure-loop mini-operator in the MCP process.

_Considered and rejected: Option A (in-process MCP reconcile of the proxy Deployment — re-invents operator watches/ownerRefs/status and collides with stateless MCP replicas), Option B (GitOps-static proxy/dummy/routes — cannot derive dummy kubeconfig and CA at runtime; NP drift is the token-replay footgun), Option C (GitOps Deployments + MCP-derived CA/dummy/NPs — splits the security boundary across two owners and still needs a half-written reconciler in the MCP)._

---

## Q2: How broad is the investigation ClusterRole?

This is the identity the proxy injects. It must be a **read-only investigation** surface, not an SA that already has `pods/exec`, VM start/stop, or `nodes/proxy`. Tokens live in the admin Secret `cli-mcp-<name>-kubeconfig`. The operator does not mint those tokens and does **not** create the investigation SA or ClusterRoleBinding (identity ownership: admin/GitOps binds identities GitOps creates).

This repo can still ship **sample** ClusterRole YAML under `config/samples/` so operator tests and catalog consumers have a starting point. First-party GitOps copies or tightens that sample; it is not a reconciler child.

A workspace note currently describes first-party investigation RBAC as `view` + extras. That line is a placeholder this question replaces.

### Option B: Custom ClusterRole only — no `view`; explicit resource list; no secrets

Hand-maintained rules: pods, logs, events, controllers, routes, networkpolicies, CRs needed for investigations, plus cluster-scoped reads investigators actually use (namespaces, PVs, CRDs, ClusterRoles, storageclasses, metrics, and on OpenShift e.g. clusteroperators). **No `secrets`**, no `pods/exec` / attach / portforward, no `nodes/proxy`, no impersonate, no VM mutate.

- **Pro:** Closes `oc get secret -A`. Least privilege vs `view`.
- **Con:** Will miss resources until someone adds them; more GitOps churn. Investigations that today `oc get secret` would break — that is intended. If a specific namespace’s files must be read, use a **separate**, namespace-pinned tool, not this proxy.

**Decision:** Option B — custom ClusterRole, no `view`, no secrets. Admin/GitOps owns the binding; this repo ships a sample only.

_Considered and rejected: Option A (`view` + extras — `view` includes `get` on Secrets), Option C (reuse a privileged SA — proxy injection would grant exec/VM power)._

---

## Q3: Require HTTP `Proxy-Authorization` in addition to NetworkPolicy?

kubectl honors `HTTPS_PROXY=http://user:pass@host:8080` and sends `Proxy-Authorization` on CONNECT. NetworkPolicy (proxy ingress) is the primary control so only sandbox pods can reach `:8080`. This would be a second factor if something in the **instance namespace** can spoof sandbox labels or if NP is mis-applied.

The operator already treats that namespace as the secret trust boundary (HMAC, kubeconfig). Proxy ingress NP + instance labels is new; claw-operator has **no** proxy-ingress NP.

### Option A: NetworkPolicy only (v1)

- **Pro:** Matches claw’s proxy auth (none) while we are already stricter on NP. Fewer secrets. kubectl/`curl` keep a simple `HTTPS_PROXY`.
- **Con:** Anyone who can run a pod with this instance’s sandbox labels in the CR namespace can use the proxy and thus the investigation token. That already implies they can create pods in that namespace (high privilege).

**Decision:** Option A — NetworkPolicy only for v1. Revisit auth if untrusted workloads in the instance namespace can set arbitrary pod labels.

_Considered and rejected: Option B (NP + proxy basic auth — extra secret in every sandbox and CONNECT auth claw does not have)._

---

## Q4: How tight is proxy egress NetworkPolicy?

Sandbox egress is proxy+DNS only. Proxy egress must reach every API server listed in the investigation kubeconfig (often `:6443`, in-cluster `:443`). Claw’s kube path adds those ports to `0.0.0.0/0` and treats L7 as the real allowlist. Claw DNS uses ports 53/5353 with `namespaceSelector: {}` (any namespace) so OpenShift `openshift-dns` and generic CoreDNS both work.

### Option A: DNS + TCP 443 and 6443 to `0.0.0.0/0` (plus IPv6 `::/0` if dual-stack)

- **Pro:** API load-balancer and PrivateLink IPs can change without NP edits. Same as claw. L7 host allowlist still 403s unknown CONNECT. Portable.
- **Con:** If L7 is buggy, the proxy pod can speak HTTPS to the internet on those ports. `namespaceSelector: {}` for DNS is slightly loose (DNS ports only).

**Decision:** Option A — DNS + TCP 443/6443 to `0.0.0.0/0` (and `::/0` if dual-stack). L7 is the real host allowlist. Copy claw DNS (53/5353, `namespaceSelector: {}`). No EgressFirewall on sandbox or proxy pods.

_Considered and rejected: Option B (reconcile-time ipBlocks — DNS TTL / NLB churn and OpenShift pain), Option C (namespace EF / DNSNames — not per-pod; hits MCP and other workloads in the instance namespace)._

---

## Q5: Allow CONNECT to raw IP addresses?

`MatchRoute` is hostname-based. `oc` uses the kubeconfig `server` URL (usually a hostname). An attacker in the sandbox can `curl -x $HTTPS_PROXY https://<api-ip>:6443` with a stolen token. If we also inject by IP, that becomes a replay path unless strip-then-inject still replaces Authorization (it would — injection is by host key). If the IP is **not** in the token map, inject fails closed (good) but CONNECT might still be allowed as a tunnel.

### Option A: Reject CONNECT unless the host matches a kubeconfig server host:port (IPs only if the kubeconfig server is an IP)

- **Pro:** No extra tunnel to “something on 6443.” Matches strip-then-inject: unknown host is 403.
- **Con:** If a cluster is only reachable by IP and kubeconfig uses a hostname, `oc` still uses the hostname (fine). Unusual kubeconfigs that mix IP and hostname need the IP as a cluster server URL.

**Decision:** Option A — allow IP CONNECT only when that `ip:port` is literally a kubeconfig `server`. Do not DNS-resolve and add IPs.

_Considered and rejected: Option B (map resolved IPs into the token map — DNS drift and shared-LB CONNECT)._

---

## Q6: Deny kube subresources at L7 (`exec` / `attach` / `portforward` / `proxy`)?

Investigation RBAC should already deny these. Bash can still *attempt* them. Claw kubernetes routes do not path-filter; `AllowedPaths` exists on the proxy for other injectors (allowlist, not denylist).

### Option B: Deny-list well-known mutating subresource path suffixes on kubernetes routes

Reject paths matching `…/exec`, `…/attach`, `…/portforward`, `…/proxy` (and impersonate is already header-stripped).

- **Pro:** Cheap belt given unconstrained bash. Survives a RoleBinding mistake.
- **Con:** Path matching on the kube API is annoying (query strings, `?command=`, SPDY). False positives possible; must test `oc logs`, `oc get --watch`, `oc explain`. New code vs claw’s allowlist.

**Decision:** Option B — L7 denylist on kubernetes routes for `exec` / `attach` / `portforward` / `proxy`. RBAC remains authoritative. Tests must keep `logs` and `watch` allowed.

_Considered and rejected: Option A (RBAC only — a mis-bound ClusterRole would make `oc exec` work through the proxy)._

---

## Q7: How much claw-operator proxy code to copy?

Goal: own image, no claw-operator release coupling. `claw-operator` is in-tree next door (`internal/proxy`): MITM CONNECT, injectors (kubernetes, bearer, none, gcp, oauth2, path_token, api_key), gateway/pathPrefix reverse-proxy mode, Slack body rewrite.

### Option B: Minimal + `bearer` injector now, still no gateway/Slack/GCP/oauth2

- **Pro:** Route-list architecture is real in v1 (kubernetes + bearer types exist; v1 ConfigMap only enables kubernetes). Curl illustration stays honest.
- **Con:** A few more files/tests unused in production v1.

**Decision:** Option B — copy MITM + kubernetes + bearer + none into `pkg/proxy`. Do not copy gateway mode, Slack, GCP, oauth2, path_token, or api_key. Keep claw’s CONNECT allow/deny and upstream TLS verification (never goproxy’s default `InsecureSkipVerify`). The operator must not import `pkg/proxy`.

_Considered and rejected: Option A (no bearer until a curl class — route-list types would be a lie in v1), Option C (copy almost whole — dead gateway/GCP/oauth2 surface)._

---

## Q8: Instance identity for labels and resource names?

**Constrained by operator Q8** (decided): CR `metadata.name` is the instance id. Labels and annotations live under `cli-mcp.redhat.com`. Children named so they fit 63 chars with CEL **name ≤ 44**.

v1 is one instance. Selectors must not be a single shared `component=sandbox` so a later instance in the **same namespace** does not share NPs.

`cli-mcp-<name>-dummy-kubeconfig` does **not** fit (70 chars at name=44). Fold sandbox egress into the existing NP `cli-mcp-<name>-sandbox` instead of a second long name.

### Option A: Follow operator Q8; `component=proxy`; suffix names

- `cli-mcp.redhat.com/instance=<CR name>` on MCP, sandbox, session Secrets, and proxy pods.
- `cli-mcp.redhat.com/component=sandbox` \| `server` \| `proxy` (`ComponentProxy` does not exist yet; only `sandbox` and `server` are in `pkg/session`).
- MITM proxy Deployment / Service / SA / NP: `cli-mcp-<name>-proxy` (same suffix pattern as `-sandbox`, `-client`, `-krp`).
- CA Secret: `cli-mcp-<name>-proxy-ca`. Dummy ConfigMap: `cli-mcp-<name>-dummy`. Routes ConfigMap: `cli-mcp-<name>-routes`.
- NPs select **instance + component**. Session list/GC stays component+instance. Sandbox egress folds into existing NP `cli-mcp-<name>-sandbox`.

- **Pro:** One label domain. NP podSelectors are obvious. Two CRs in one namespace cannot share proxies. Stays inside CEL 44.
- **Con:** Dummy ConfigMap name is slightly less obvious than `…-dummy-kubeconfig`.

**Decision:** Option A — `component=proxy` and the suffix names above. Do not tighten CEL below 44.

_Considered and rejected: Option B (shared component-only selector — already rejected by operator Q8), Option C (extra `cli-mcp-class` label — CR name is the instance id)._

---

## Q9: What ServiceAccount do sandbox pods run as?

**Constrained by operator Q12** (decided and implemented): dedicated sandbox SA `cli-mcp-<name>-sandbox`, no RoleBindings, `automountServiceAccountToken: false`. Investigation tokens must not be the pod’s projected SA token.

This question only confirms that the investigation subject exists **only** as ClusterRoleBinding subjects whose tokens are minted into the proxy’s kubeconfig Secret — and that we add a **MITM proxy** SA with the same shape (no RoleBindings, automount false). That SA is not the kube-rbac-proxy sidecar (sidecar uses the MCP pod SA `cli-mcp-<name>`).

### Option A: Dedicated `cli-mcp-<name>-sandbox` SA, no RoleBindings, `automountServiceAccountToken: false`

- **Pro:** Already shipped. Compromised sandbox gets no in-cluster identity. OpenShift still has an SA for SCC. Proxy SA `cli-mcp-<name>-proxy` follows the same rule (upstream auth is the kubeconfig mount).
- **Con:** One more SA (proxy) — cheap.

**Decision:** Option A — keep `cli-mcp-<name>-sandbox`; add `cli-mcp-<name>-proxy` with no RoleBindings and automount false. Investigation tokens live only in the admin kubeconfig Secret on the proxy, never as a projected pod token.

_Considered and rejected: Option B (investigation SA on the sandbox pod — operator Q12 already rejected this), Option C (namespace default SA — footgun if anyone binds it)._

---

## Q10: Proxy CA lifecycle?

MITM requires a CA the dummy kubeconfig trusts. Claw generates a P-256 ECDSA CA once, stores it in a Secret, and never rotates unless the Secret is deleted. This operator’s HMAC Secret is the closer in-tree pattern: create-if-missing, **never overwrite** a present Secret (empty/wrong key stays `SecretKeysInvalid`).

### Option A: Generate-once (create-if-not-exists), 10-year lifetime, no automatic rotation

- **Pro:** Same as HMAC. Dummy kubeconfig and running sandboxes stay valid. MCP/proxy replicas do not flip-flop CAs.
- **Con:** Compromise of `ca.key` means forging API-looking certs to sandboxes (they can only talk to the proxy anyway). Rotation is a documented break-glass: delete CA Secret + dummy CM, bounce proxy, idle-GC sandboxes.

**Decision:** Option A — generate-once in `cli-mcp-<name>-proxy-ca` (P-256 ECDSA, IsCA, 10y). Never overwrite a present Secret. Empty `ca.crt`/`ca.key` → not Ready (`SecretKeysInvalid`), do not silently mint into a pre-created empty object.

_Considered and rejected: Option B (cert-manager / service CA — cannot sign MITM leafs for `api.<cluster>`), Option C (new CA every restart — breaks warm pool and live sessions)._

---

## Q11: How does the proxy pick up investigation kubeconfig rotation?

An ExternalSecret (or equivalent) may rotate tokens. Claw stamps the Secret `resourceVersion` on the proxy Deployment to force a rollout; the proxy reads kubeconfig at **startup** only (no file watch).

This operator **already watches** Secrets named `cli-mcp-<name>-kubeconfig` (`mapSecret` / `instanceFromAdminSecret`) and stamps HMAC RV on the **MCP** pod template. Proxy can use the same pattern.

### Option B: Operator watches the Secret and patches the proxy Deployment annotation

- **Pro:** Self-contained. Dummy + routes + proxy stay in lockstep. Matches HMAC RV on MCP. Secret watch already exists.
- **Con:** Operator needs to apply the proxy Deployment (already in scope).

**Decision:** Option B — stamp `cli-mcp.redhat.com/kubeconfig-resource-version` (and CA RV) on the proxy pod template; rewrite dummy + routes on the same reconcile. Do not assume Reloader.

_Considered and rejected: Option A (Reloader — not a required cluster install), Option C (inotify reload — races and extra proxy complexity)._

---

## Q12: Fail-closed if proxy, dummy kubeconfig, or NPs are not ready?

If sandbox pods are created before egress NP exists, they have unrestricted egress (the instance namespace has no default-deny today). That window is the original bug.

MCP does **not** watch the CR (operator Q9) and will on-demand-create whenever it is running. Aggregate `Ready=false` does **not** stop that. Pool mutate today runs whenever `applyChildren` succeeds.

On upgrade from the current operator, assigned pods still mount the real Secret until DELETE / idle GC / CR delete (overlay leaves assigned pods). Applying sandbox egress NP immediately cuts their direct API path (fail-closed for replay even while the token sits on disk).

### Option A: Operator-enforced gate — no pool mutate without dummy+egress NP; apply those children first; leave assigned pods on overlay

- Apply CA, dummy, routes, proxy Service/Deployment, sandbox NP **with egress**, proxy NP **before** pool create and before relying on new MCP flags.
- `reconcilePool(..., mutate)` stays false until dummy ConfigMap exists and the sandbox NP has egress.
- Unassigned overlay rebuild picks up dummy+`HTTPS_PROXY`.
- Assigned sessions keep the old spec until idle/DELETE (today’s overlay rule). Document the upgrade window: token may remain on disk in those pods; egress NP should already block direct API; through-proxy replay still strip-then-injects.
- **Pro:** Matches two-writer reality. No MCP infra watch. Install cannot create open-egress sandboxes once P3 rolls out.
- **Con:** Live assigned sessions during the first proxy upgrade keep a disk copy of the old Secret until idle GC. MCP replicas that have not rolled yet can still *attempt* Secret-mounted creates; with egress NP already on, those pods cannot reach the API except via a proxy they are not configured to use.

**Decision:** Option A — operator gate on dummy+egress NP; do not kill assigned sessions on cutover. First-party can drain sessions before the catalog bump if they want a clean cutover.

_Considered and rejected: Option B (GitOps ordering only — MCP is not GitOps-created), Option C (namespace default-deny — easy to outage MCP and other workloads), Option D (scale MCP to 0 and delete assigned — hard-kills live bash)._

---

## Q13: Does the CRD grow `spec.proxy`?

Operator Q14 omitted `spec.proxy` so this design could shape it. Routes, CA, and dummy **must** be derived from the live kubeconfig + proxy CA (Q1). The proxy image is ours (`RELATED_IMAGE_PROXY`), same as MCP (`RELATED_IMAGE_SERVER`) — not a per-CR pin.

### Option A: No spec fields. Hardcode proxy pod resources; image from operator env

- **Pro:** Smallest CRD delta. No frozen injector/route API. Matches “admin does not hand-maintain the proxy Deployment.”
- **Con:** Cannot set proxy CPU/memory per instance without a later CRD add.

**Decision:** Option A — no `spec.proxy` / `spec.proxyContainer` in v1. Image from `RELATED_IMAGE_PROXY`. Proxy resources use DefaultConfig-like requests/limits. Additive `spec.proxyContainer` can wait until someone needs it. Do not add `spec.proxy.routes`.

_Considered and rejected: Option B (`spec.proxyContainer` now — unused by most installs), Option C (full `spec.proxy` — freezes derived dummy/routes as admin YAML)._

---

## Q14: What does Ready do with a kubeconfig that has a key but is not token-only?

Today Ready only checks Secret `cli-mcp-<name>-kubeconfig` exists with a non-empty `kubeconfig` key. It does **not** parse YAML. The proxy **must** parse: reject client certs / exec / auth-provider / basic auth; build dummy + routes.

A present-but-unusable kubeconfig would otherwise look Ready while every `oc` through the proxy fails closed — or worse, if we skipped validation, we might mount a dummy that still embeds a client cert.

### Option A: Parse in Ready. New reason `KubeconfigInvalid` (do not go Ready)

- **Pro:** Fail closed at the instance gate. Matches HMAC empty-key → `SecretKeysInvalid`. Operators and GitOps see why bash cannot `oc`.
- **Con:** Ready now depends on kubeconfig schema, not only key presence (operator Q15 originally avoided parse). Must not log tokens.

**Decision:** Option A — parse kubeconfig in Ready. Missing/empty key stays `SecretKeysInvalid`. Unparseable or non-token-only → `KubeconfigInvalid`. Still no TLS cert parse. Never emit token material in condition messages.

_Considered and rejected: Option B (vague `ChildrenNotReady`), Option C (Ready with a broken kubeconfig — sandboxes fail at runtime)._

---

## Q15: Does local `cmd/server` still run without the proxy?

Operator Q6/Q9: `cmd/server` stays flag-driven without the operator (unit tests, `go run`, kind without a CR). In-cluster after P3, the operator always passes dummy + proxy URL.

### Option A: Dual path — XOR flags; dummy+proxy if set, else today’s Secret mount

Today `--kubeconfig-secret` is required. Change validation to **exactly one** of: `--kubeconfig-secret`, or `--dummy-kubeconfig-configmap` **and** `--proxy-url`. Passing both is an error (no silent Secret fallback).

- **Pro:** Existing `pkg/session` / MCP tests keep working. Local `oc` against a real cluster still possible. In-cluster operator always takes the dummy path.
- **Con:** Two volume shapes in `BuildBasePodSpec`. Tests must cover both. A mis-wired operator that omits the new flags would fail MCP startup (good) rather than mount the real Secret.

**Decision:** Option A — XOR flags. P3 always passes dummy+proxy URL and never `--kubeconfig-secret`. Empty `RELATED_IMAGE_PROXY` is apply-fail, not Secret fallback.

_Considered and rejected: Option B (always require dummy — breaks local/unit tests), Option C (delete Secret-mount code — same as B for tests)._
