# CLI MCP — credential-isolating proxy — design questions

**Status:** Paused after Q1 — remaining questions wait on the operator **implementation**  
**Related:** [Design document](credential-proxy-design.md) · [Operator HOW](cli-mcp-operator-design.md) · [Operator questions](cli-mcp-operator-questions.md)

Q1 is decided. **Q2–Q12 are on pause** until the CLI MCP Operator is implemented. This proxy document is the component/security inventory (WHAT). The operator is HOW those objects are managed. After that implementation, return here and finish these questions with controller ownership in mind.

This is an **open-source Kubernetes operator**. First-party internal deploy is one catalog consumer; do not bake that environment into the API.

---

## Q1: Who owns the proxy stack vs derived objects?

The umbrella analysis said the MCP server would create the proxy Deployment, route ConfigMap, CA, dummy kubeconfig, and NetworkPolicies. That is a mini-operator inside a process that must stay **stateless and multi-replica** for `bash`. Traditional operators are leader-elected singletons. Dummy kubeconfig and routes **must** be derived from the live investigation Secret + proxy CA (not frozen in git). Child objects also need watches, ownerRefs, and status — which a 30s ensure loop in `cmd/server` would re-invent.

### Option D: Real Kubernetes operator (CLI MCP Operator)

A CRD instance (one CR = one MCP instance / class) is the API. A leader-elected operator Deployment watches it and bootstraps instance infrastructure: MCP server Deployment, proxy Deployment+ClusterIP Service, CA, route ConfigMap, dummy kubeconfig, NetworkPolicies, sandbox SA, and related RBAC wiring.

The MCP server stays a horizontally scaled data plane: `bash`, per-session pods, HMAC claim/create/`/exec`. It does **not** reconcile the proxy stack, the MCP Deployment, warm-pool size, or idle GC (those are the operator — see operator Q5). Sessions are not CRs.

GitOps installs the operator once and applies `CliMcpInstance` (name TBD) objects. It does not hand-maintain the proxy Deployment.

- **Pro:** Proper watches (deleted/edited proxy comes back), ownerRefs/GC, status (`ProxyReady`), validation on spec (sandbox image, proxy config, warm pool). A second class later is another CR, not a snowflake Deployment. Matches claw-operator’s split (operator owns children; workload process is not the controller). Resolves singleton-vs-stateless: operator is the singleton; MCP replicas stay stateless.
- **Con:** CRD, manager, envtest, operator RBAC — larger than “apply() in `cmd/server`.” Two Deployments to run (operator + MCP). Operator design must reserve spec/status for proxy children even if proxy ships in a later phase.

**Decision:** Option D — real operator. This proxy design keeps the security/topology WHAT; operator design will be HOW. Do not put an ensure-loop mini-operator in the MCP process.

_Considered and rejected: Option A (in-process MCP reconcile of the proxy Deployment — re-invents operator watches/ownerRefs/status and collides with stateless MCP replicas), Option B (GitOps-static proxy/dummy/routes — cannot derive dummy kubeconfig and CA at runtime; NP drift is the token-replay footgun), Option C (GitOps Deployments + MCP-derived CA/dummy/NPs — splits the security boundary across two owners and still needs a half-written reconciler in the MCP)._

---

**Pause:** Q2–Q12 below are unchanged in *decision status* and **not** being walked through until the operator is implemented. Wording is aligned with the operator HOW (Final): `cli-mcp.redhat.com` labels, dedicated sandbox SA, operator owns children.

## Q2: How broad is the investigation ClusterRole?

This is the identity the proxy injects. It must be a **read-only investigation** surface, not an SA that already has `pods/exec`, VM start/stop, or `nodes/proxy`. Tokens live in a **new** Secret. The operator does not mint those tokens (admin-provided kubeconfig).

### Option A: Bind `view` + a dedicated `cli-mcp-investigation-readonly` ClusterRole

`view` for namespaced get/list/watch. Extra ClusterRole for cluster-scoped reads investigators actually use (namespaces, PVs, CRDs, ClusterRoles, storageclasses, metrics, and on OpenShift e.g. clusteroperators) **minus** `nodes/proxy`, **minus** any exec/attach/portforward, **no secrets**.

- **Pro:** Close to a typical cluster-wide `oc` investigation view; a client replacing a broader kubernetes MCP does not silently lose cluster-scoped reads.
- **Con:** `view` is broad (all namespaced resources including user Secrets **in every namespace**). `view` includes `get` on `secrets`. That is a real data leak via `oc get secret` even with no exec.

### Option B: Custom ClusterRole only — no `view`; explicit resource list; no secrets

Hand-maintained rules: pods, logs, events, controllers, routes, networkpolicies, CRs needed for investigations, cluster-scoped reads from Option A, **no `secrets`**, no `pods/exec`, no `nodes/proxy`.

- **Pro:** Closes `oc get secret -A` / `oc get secret … -o yaml`. Least privilege vs `view`.
- **Con:** Will miss resources until someone adds them; more GitOps churn. Investigations that today `oc get secret` would break — that is intended. If a specific namespace’s files must be read, use a **separate**, namespace-pinned tool, not this proxy.

### Option C: Reuse an existing privileged SA / kubeconfig but rely on no-exec + proxy

- **Pro:** No new tokens.
- **Con:** If that SA has exec or VM power, proxy injection would give the bash sandbox **exec**. Violates the stated no-exec requirement.

**Recommendation:** Option B. Do not bind `view` (it includes secrets). Do not reuse an SA that already has exec / VM mutate / `nodes/proxy`. Start from the *effective* investigation surface you need, strip secrets/exec/`nodes/proxy`/impersonate/VM mutate, then add resources only when a real investigation hits a gap. Verify with `kubectl auth can-i --list`.

---

## Q3: Require HTTP `Proxy-Authorization` in addition to NetworkPolicy?

kubectl honors `HTTPS_PROXY=http://user:pass@host:8080` and sends `Proxy-Authorization` on CONNECT. NetworkPolicy (proxy ingress) is the primary control so only sandbox pods can reach `:8080`. This would be a second factor if something in the **instance namespace** can spoof sandbox labels or if NP is mis-applied.

### Option A: NetworkPolicy only (v1)

- **Pro:** Matches claw-operator today (claw has **no** proxy-ingress NP and no proxy basic auth — we are already stricter on NP). Fewer secrets. kubectl/`curl` keep a simple `HTTPS_PROXY`.
- **Con:** Anyone who can run a pod with this instance’s sandbox labels in the CR namespace can use the proxy and thus the investigation token. That already implies they can create pods in that namespace (high privilege).

### Option B: NP + proxy basic auth (secret in sandbox env)

- **Pro:** Defense in depth if labels are copied or NP selectors go wrong.
- **Con:** Secret mounted in every sandbox (readable by bash — but it only authorizes *use of the already-dummy path*, not a cluster token). goproxy must enforce CONNECT auth; claw does not. More moving parts for v1.

**Recommendation:** Option A for v1. Proxy ingress NP + ClusterIP + instance labels. Revisit auth if we ever run untrusted workloads in the instance namespace that can set arbitrary pod labels.

---

## Q4: How tight is proxy egress NetworkPolicy?

Sandbox egress is proxy+DNS only. Proxy egress must reach every API server listed in the investigation kubeconfig (often `:6443`, in-cluster `:443`). Claw’s kube path adds those ports to `0.0.0.0/0` and treats L7 as the real allowlist.

### Option A: DNS + TCP 443 and 6443 to `0.0.0.0/0`

- **Pro:** API load-balancer and PrivateLink IPs can change without NP edits. Same as claw. L7 host allowlist still 403s unknown CONNECT.
- **Con:** If L7 is buggy, the proxy pod can speak HTTPS to the internet. `0.0.0.0/0` does not cover IPv6.

### Option B: Resolve kubeconfig hostnames at reconcile time; NP `ipBlock` CIDRs

- **Pro:** Proxy cannot talk to arbitrary IPs even if L7 fails.
- **Con:** DNS TTL / NLB change → outage until re-reconcile. Need to handle multiple A records, IPv6, and `kubernetes.default.svc` cluster IPs. Painful on OpenShift.

### Option C: OpenShift EgressFirewall / DNSNames on the **proxy** namespace or pod

- **Pro:** Hostname-level egress at CNI.
- **Con:** EF is namespace-scoped on OpenShift, not per-pod. Would affect the MCP server and any other workloads in the instance namespace if applied to the whole namespace. Per-pod FQDN policy needs Cilium (not the cluster default). Easy to get wrong; the umbrella analysis already rejected EF on **sandbox** pods together with the proxy.

**Recommendation:** Option A for v1, plus IPv6 `::/0` on the same ports if the cluster is dual-stack. Document L7 as the real host allowlist. Do not put EF on sandbox pods.

---

## Q5: Allow CONNECT to raw IP addresses?

`MatchRoute` is hostname-based. `oc` uses the kubeconfig `server` URL (usually a hostname). An attacker in the sandbox can `curl -x $HTTPS_PROXY https://<api-ip>:6443` with a stolen token. If we also inject by IP, that becomes a replay path unless strip-then-inject still replaces Authorization (it would — injection is by host key). If the IP is **not** in the token map, inject fails closed (good) but CONNECT might still be allowed as a tunnel.

### Option A: Reject CONNECT unless the host matches a kubeconfig server host:port (IPs only if the kubeconfig server is an IP)

- **Pro:** No extra tunnel to “something on 6443.” Matches strip-then-inject: unknown host is 403.
- **Con:** If a cluster is only reachable by IP and kubeconfig uses a hostname, `oc` still uses the hostname (fine). Unusual kubeconfigs that mix IP and hostname need the IP as a cluster server URL.

### Option B: Also map resolved IPs to the same token (inject on IP CONNECT)

- **Pro:** `curl https://<resolved-ip>` works like `oc`.
- **Con:** DNS/IP drift; easier to accidentally allow CONNECT to a shared LB IP that fronts more than the API. More code.

**Recommendation:** Option A. Allow IP CONNECT only when that `ip:port` is literally a kubeconfig `server`. Do not DNS-resolve and add IPs.

---

## Q6: Deny kube subresources at L7 (`exec` / `attach` / `portforward` / `proxy`)?

Investigation RBAC should already deny these. Bash can still *attempt* them. Claw kubernetes routes do not path-filter; `AllowedPaths` exists on the proxy for other injectors.

### Option A: RBAC only

- **Pro:** One source of truth. No denylist to maintain (`pods/ephemeralcontainers`, impersonate already stripped as headers, `nodes/proxy`, …).
- **Con:** Mis-bound ClusterRole + `oc exec` is instant cluster-admin-adjacent in user namespaces.

### Option B: Deny-list well-known mutating subresource path suffixes on kubernetes routes

Reject paths matching `…/exec`, `…/attach`, `…/portforward`, `…/proxy` (and impersonate is already header-stripped).

- **Pro:** Cheap belt given unconstrained bash. Survives a RoleBinding mistake.
- **Con:** Path matching on the kube API is annoying (query strings, `?command=`, SPDY). False positives possible; must test `oc logs`, `oc get --watch`, `oc explain`.

**Recommendation:** Option B as a small denylist with tests for `logs`/`watch` still allowed. RBAC remains authoritative; this is belt-and-suspenders for the exact bypass we are designing against (`exec`).

---

## Q7: How much claw-operator proxy code to copy?

Goal: own image, no claw-operator release coupling. claw’s `internal/proxy` also has gateway/pathPrefix reverse-proxy mode, Slack body rewrite, GCP token vending, oauth2, path_token, api_key.

### Option A: Minimal — MITM CONNECT + kubernetes injector + `none` + StripAuthHeaders + route matching + CA pool

- **Pro:** Smallest attack surface and test matrix. Enough for v1 kube and for a later curl class (`none` or `bearer` can be added then).
- **Con:** Harder to diff against claw later; a later curl class may need `bearer` immediately.

### Option B: Minimal + `bearer` injector now, still no gateway/Slack/GCP/oauth2

- **Pro:** Route-list architecture is real in v1 (kubernetes + bearer types exist; v1 ConfigMap only enables kubernetes). Curl illustration stays honest.
- **Con:** A few more files/tests unused in production v1.

### Option C: Copy the claw package almost whole, delete Slack rewrite only

- **Pro:** Easier to pull claw bugfixes.
- **Con:** Dead injectors, gateway mode we do not want (clients would skip CONNECT), GCP dummy-token behavior is confusing in this threat model.

**Recommendation:** Option B. Copy MITM + kubernetes + bearer + none. Do not copy gateway mode, Slack, GCP, oauth2, path_token, api_key. Keep claw’s CONNECT allow/deny and upstream TLS verification (never goproxy’s default `InsecureSkipVerify`).

---

## Q8: Instance identity for labels and resource names?

**Constrained by operator Q8** (decided): CR `metadata.name` is the instance id. Labels and annotations live under `cli-mcp.redhat.com`. As-built `tarsy.redhat.com/*` is dropped (nothing in production; no migration). Children named `cli-mcp-<name>`.

v1 is one instance. Selectors must not be a single shared `component=sandbox` so a later instance in the **same namespace** does not share NPs.

When this question is resumed, the remaining work is proxy-specific labels, not a new domain:

### Option A: Follow operator Q8; proxy pods get `component=proxy`

- `cli-mcp.redhat.com/instance=<CR name>` on MCP, sandbox, session Secrets, and proxy pods.
- `cli-mcp.redhat.com/component=sandbox` \| `server` \| `proxy`.
- Proxy Service / Deployment named `cli-mcp-proxy-<name>` or `cli-mcp-<name>-proxy` (pick one when implementing; must fit 63 chars with the sandbox SA suffix already reserved).
- NPs and proxy ingress select **instance + component**. Session list/GC stays component+instance as the operator phase.

- **Pro:** One label domain. NP podSelectors are obvious. Two CRs in one namespace cannot share proxies.
- **Con:** None beyond the operator phase already accepted.

### Option B: Reuse a single `component=cli-mcp-sandbox` value and encode class in the value

- **Pro:** No extra instance key.
- **Con:** Breaks operator-phase meaning of `component=sandbox` (session manager, warm pool, idle GC). Rejected by operator Q8.

### Option C: A separate `cli-mcp-class` label besides instance

- **Pro:** Could distinguish class vs instance if we ever run two `oc` CRs.
- **Con:** Two labels to document; CR name already is the instance id.

**Recommendation:** Option A. Do not re-open `tarsy.redhat.com` or a shared component-only selector.

---

## Q9: What ServiceAccount do sandbox pods run as?

**Constrained by operator Q12** (decided): dedicated sandbox SA, no RoleBindings, `automountServiceAccountToken: false`. Investigation tokens must not be the pod’s projected SA token. This question is kept so the proxy pass can confirm the SA name and that the investigation subject exists **only** as ClusterRoleBinding subjects whose tokens are minted into the proxy’s kubeconfig Secret.

### Option A: Dedicated `cli-mcp-<name>-sandbox` SA, no RoleBindings, `automountServiceAccountToken: false`

- **Pro:** Clear split. Compromised sandbox gets no in-cluster identity. OpenShift still has an SA for SCC.
- **Con:** One more object (already created in the operator phase).

### Option B: Investigation SA on the pod with automount false; tokens only in the proxy kubeconfig

- **Pro:** Fewer SAs.
- **Con:** Name implies the sandbox *is* the investigation identity. Accidental automount true (revert/bug) immediately projects a useful in-cluster token, bypassing the dummy kubeconfig. Operator Q12 already rejected this.

### Option C: `automountServiceAccountToken: false` and empty `serviceAccountName` (default SA in namespace)

- **Pro:** No extra SA.
- **Con:** Namespace `default` SA is a footgun if anyone binds it. Less explicit. Operator Q12 already rejected this.

**Recommendation:** Option A — same as operator Q12. Investigation SA exists only as the subject of ClusterRoleBindings whose tokens are minted into the proxy’s kubeconfig Secret (typically via External Secrets or equivalent), never mounted on sandbox pods.

---

## Q10: Proxy CA lifecycle?

MITM requires a CA the dummy kubeconfig trusts. Claw generates a P-256 ECDSA CA once, stores it in a Secret, and never rotates unless the Secret is deleted.

### Option A: Generate-once (create-if-not-exists), 10-year lifetime, no automatic rotation

- **Pro:** Same as claw. Dummy kubeconfig and running sandboxes stay valid. MCP replicas do not flip-flop CAs.
- **Con:** Compromise of `ca.key` means forging API-looking certs to sandboxes (they can only talk to the proxy anyway). Rotation is a documented break-glass: delete CA Secret + dummy CM, bounce proxy, idle-GC sandboxes.

### Option B: cert-manager (or OpenShift service CA)

- **Pro:** Rotation/policy exists in platform.
- **Con:** Service CA cannot sign arbitrary MITM leafs for `api.<cluster>`. cert-manager is another dependency in the instance namespace for one Secret.

### Option C: New CA every MCP restart

- **Pro:** Short-lived.
- **Con:** Breaks warm pool and live sessions; multi-replica races.

**Recommendation:** Option A. Generate-once in the CA Secret (**operator**, Q1). Copy claw’s CA template (IsCA, KeyUsageCertSign, ECDSA P-256).

---

## Q11: How does the proxy pick up investigation kubeconfig rotation?

An ExternalSecret (or equivalent) may rotate tokens. Claw stamps the Secret `resourceVersion` on the proxy Deployment to force a rollout; the proxy reads kubeconfig at **startup** only (no file watch).

### Option A: Reloader / `secret.reloader.stakater.com` annotation in GitOps

- **Pro:** No operator code. Common on OpenShift.
- **Con:** Depends on Reloader being installed in the cluster (confirm). Dummy kubeconfig cluster *list* also needs refresh if servers were added — operator reconcile on an interval can rewrite dummy/routes; proxy still needs restart to reload tokens.

### Option B: Operator watches the Secret and patches the proxy Deployment annotation

- **Pro:** Self-contained. Dummy + routes + proxy stay in lockstep.
- **Con:** Operator needs patch on the proxy Deployment. More controller behavior (acceptable: Q1 already made the operator the owner).

### Option C: Proxy watches the kubeconfig file (inotify) and reloads

- **Pro:** No rollout.
- **Con:** Reload races, token map mutex, not how claw works; more proxy complexity.

**Recommendation:** Option B if Reloader is not already a standard in the target cluster; otherwise Option A plus the operator periodically rewriting dummy/routes. Default to **Option B** unless GitOps owners confirm Reloader. Tokens must not stay stale after rotation.

---

## Q12: Fail-closed if proxy, dummy kubeconfig, or NPs are not ready?

If sandbox pods are created before egress NP exists, they have unrestricted egress (the instance namespace has no default-deny today). That window is the original bug.

Operator Q5 moved warm pool to the operator; MCP still creates on-demand session pods. Both paths must honor this gate. Operator Q15 `Ready` should include proxy children in the proxy pass.

### Option A: Do not create/claim sandbox pods until Ready proxy endpoints, dummy ConfigMap, and the four NPs are observed

- **Pro:** No open-egress sandbox even on first deploy / MCP crashloop.
- **Con:** First `bash` call waits on proxy readiness (acceptable). Warm pool must not pre-create pods either until the same gate passes.

### Option B: GitOps ordering only (proxy+NPs in the same Argo app, hope apply order is enough)

- **Pro:** No extra code.
- **Con:** Kubernetes does not give you transactional multi-resource apply. A race on first rollout is likely.

### Option C: Namespace default-deny NetworkPolicy in GitOps, then allow-lists

- **Pro:** Even a buggy operator/MCP cannot create a sandbox with open egress.
- **Con:** Default-deny in the instance namespace would break the MCP client, other MCP servers, observability, and anything else in that namespace unless carefully namespaced by podSelector. Easy to outage the whole namespace.

**Recommendation:** Option A. Gate both on-demand create and warm-pool replenish. Do not namespace-wide default-deny the instance namespace.
