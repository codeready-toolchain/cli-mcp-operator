# CLI MCP — credential-isolating proxy — design questions

**Status:** Decisions recorded (Q1–Q11)
**Related:** [Design document](credential-proxy-design.md) · [Operator HOW](cli-mcp-operator-design.md)

Each question has options with trade-offs and a recorded decision. The matching design is [credential-proxy-design.md](credential-proxy-design.md) (**Final**).

This is the **proxy pass** of the already-implemented CLI MCP Operator. The operator is HOW children are managed. These questions are only the residual decisions. Do not re-open operator Q1–Q16.

**Already locked (not questions):** operator owns proxy children; MCP does not watch the CR and does not run an infra ensure loop; no top-level `spec.investigationKubeconfigSecretRef` (optional `secretName` on a kubernetes **target** is Q1); labels `cli-mcp.redhat.com` with `instance=<CR name>` and `component`; dedicated sandbox SA, no RoleBindings, `automountServiceAccountToken: false`; CA is HMAC-style generate-once (never overwrite a present Secret; empty keys → `SecretKeysInvalid`); kubeconfig Secret rotation is HMAC/krp-style `resourceVersion` stamp on the proxy pod template; `RELATED_IMAGE_PROXY` (not a spec field); proxy `replicas: 1` with `maxUnavailable: 0` / `maxSurge: 1` (Q11); first-party GitOps is a catalog consumer — the operator does not mint investigation tokens or ClusterRoles.

This is an **open-source Kubernetes operator**. First-party internal deploy is one catalog consumer; do not bake that environment into the API.

---

## Q1: How does the CR declare what the proxy may reach?

There is no `spec.sandbox.type`. Class is still `spec.sandbox.image` (Q16). This list is **proxy policy**: which hosts this instance may `CONNECT` to, and whether the proxy injects an identity. It is not a claw-style `credentials[]` junk drawer (one OpenClaw gateway, many injectors, union allowlist). One CR still gets one proxy; two CRs do not share it.

Operator Q14 omitted an empty `spec.proxy` stub. This pass adds the real field.

### Option A: `spec.proxy.targets[]` — types `kubernetes` and `allowlist`; optional kubernetes `secretName`

Every instance gets proxy children (CA, proxy Deployment/Service, NPs, sandbox egress). What differs is **routes, dummy kubeconfig, and which Secrets Ready waits on**.

```yaml
spec:
  sandbox: {}                    # image omitted → shipped oc class
  proxy:
    targets:
      - type: kubernetes         # sample oc: omit secretName
        # secretName: my-kube    # optional; same namespace; key kubeconfig
      # OSS / later curl — first-party oc omits this:
      # - type: allowlist
      #   domains:
      #     - api.example.com      # :443 implied
      #     - registry.example.com:8443
```

- **`type` enum:** `kubernetes` | `allowlist`. Closed **injector** set (code in `cli-mcp-proxy`), not a closed sandbox-class enum.
- **`kubernetes`:** allowlist = kubeconfig cluster `server` URLs; injector injects tokens. Dummy kubeconfig mounted on sandboxes. MITM + strip-then-inject.
- **`allowlist`:** explicit `domains` (host or `host:port`; bare host → `:443`). Injector is claw `none`: MITM, strip auth/impersonate, inject nothing. No Secret. Implemented in this pass (OSS); first-party `oc` does not use it. Sandboxes trust the proxy CA (`SSL_CERT_FILE` / equivalent) so `curl` works — including on the oc class.
- **`secretName`:** optional, **kubernetes only**. Empty → conventional `cli-mcp-<name>-kubeconfig`. Set → that Secret in the **CR namespace**. Data key is always `kubeconfig` (no `secretRef.key`). Admin creates the Secret (GitOps, External Secrets, `kubectl`). The operator **Gets** it and mounts it on the proxy. It does **not** mint tokens, create the Secret, or `ownerRef` it.
- **Cardinality:** `spec.proxy.targets` required, min 1 (no omitted-`proxy` default, no proxy-less CR). v1: **at most one** `type: kubernetes` (keeps the default name unambiguous). Many `allowlist` rows allowed. Mixing kubernetes + allowlist on one CR is that class’s union (allowed). First-party `oc` is kubernetes only. Sample CR gains an explicit kubernetes target in the cutover PR.
- **CEL:** `allowlist` requires non-empty `domains`; `kubernetes` must not set `domains`; `secretName` only on `kubernetes` (DNS-1123 label). `domains` are literal `host` or `host:port` (no `*.`, no leading-dot suffix).
- **Ready:** kubernetes row → that Secret present, key non-empty, token-only parse ([Q4](credential-proxy-questions.md)). Allowlist-only CR → no kubeconfig Secret, no dummy kubeconfig mount. Empty/wrong-key still `SecretsNotFound` / `SecretKeysInvalid` when a kubernetes target exists.
- **Watch:** enqueue on the **effective** Secret name (default or `secretName`), not a hardcoded suffix match only.

- **Pro:** Image stays the sandbox class; targets stay proxy policy. OSS can ship an allowlist-only CR without a fake kubeconfig. Default Secret name keeps today’s sample. Optional `secretName` is ceremony only when the admin’s Secret is not the conventional name. MITM on allowlist means stolen tokens cannot be replayed to allowed HTTP hosts.
- **Con:** CRD grows `spec.proxy`. Two instance shapes (with/without dummy kubeconfig). Mixing kubernetes + allowlist on one CR is a union for that sandbox (discipline: first-party `oc` must not add extra domains).

**Decision:** Option A — `spec.proxy.targets[]` with `kubernetes` and `allowlist`; optional kubernetes `secretName` (default `cli-mcp-<name>-kubeconfig`); operator reads the Secret, does not mint it; at most one kubernetes target; every instance still gets a proxy + sandbox egress.

_Considered and rejected: Option A original (every CR requires conventional kubeconfig, no `spec.proxy` — freezes the API as oc-only; fights Q16), Option B (infer proxy from Secret presence — missing Secret looks like “no proxy” instead of not Ready), Option C (`spec.proxy.enabled` stub — Q14), top-level `spec.credentials[]` (wrong name for allowlist rows; claw’s union-on-one-gateway model), `spec.sandbox.type` enum (Q16), CRD-only `allowlist` with no implementation (empty stub), operator-created investigation Secret / minted tokens, cross-namespace `secretRef`, `secretRef.key` (key stays `kubeconfig`)._

---

## Q2: How are proxy children named (DNS-1123 vs the 44-character CR limit)?

CEL already caps `metadata.name` at 44 so `cli-mcp-<name>-kubeconfig` ≤ 63. That Secret name stays. New objects must fit without lowering the cap.

As-built already reuses one DNS name across kinds: `cli-mcp-oc` is Deployment + Service + SA; `cli-mcp-oc-sandbox` is SA + NetworkPolicy.

Suffixes that **do not** fit at name length 44: `-dummy-kubeconfig` (69), `-proxy-config` (65), `-kube-config` (64). `-proxy` (58) and `-proxy-ca` (61) fit.

### Option A: Pack and reuse names

| Object | Name |
|---|---|
| Proxy Deployment, ClusterIP Service, SA, ConfigMap, NetworkPolicy | `cli-mcp-<name>-proxy` |
| Proxy CA Secret | `cli-mcp-<name>-proxy-ca` (`ca.crt`, `ca.key`) |
| Dummy kubeconfig + routes | ConfigMap `cli-mcp-<name>-proxy` data `kubeconfig` + `proxy-config.json` |
| Sandbox NetworkPolicy | existing `cli-mcp-<name>-sandbox` — **add Egress** (ingress `:8090` stays) |

- **Pro:** No CEL change. Same naming pattern as today. One ConfigMap for dummy + routes so they cannot drift. One sandbox NP so fail-closed can GET a single object.
- **Con:** Five kinds share `cli-mcp-<name>-proxy`. `kubectl get all` is noisier. Combining ingress+egress on the sandbox NP is a behavior change of an existing child (must not drop `:8090` from MCP).

**Decision:** Option A — pack and reuse names. Do not lower CEL 44. Dummy + routes share ConfigMap `cli-mcp-<name>-proxy` (dummy key only with a kubernetes target). Sandbox NP `cli-mcp-<name>-sandbox` gains Egress; keep ingress `:8090`.

_Considered and rejected: Option B (opaque short suffixes; dummy/routes can drift), Option C (lower the published CR name cap)._

---

## Q3: What ServiceAccount do proxy pods run as?

The proxy mounts the admin kubeconfig Secret and the CA Secret as volumes. It does not need in-cluster API access. The MCP SA can create/delete pods and session Secrets. The sandbox SA has no RoleBindings.

### Option A: Dedicated SA `cli-mcp-<name>-proxy`, no RoleBindings, `automountServiceAccountToken: false`

Same name as the proxy Deployment (Q2). OpenShift still has an SA for SCC.

- **Pro:** Matches sandbox. Compromised proxy process gets no in-cluster identity. Accidental RoleBinding on the MCP SA does not apply.
- **Con:** One more object (same DNS name as the Deployment).

**Decision:** Option A — dedicated proxy SA; no RoleBindings; `automountServiceAccountToken: false`.

_Considered and rejected: Option B (reuse sandbox SA — a future sandbox binding would cover the proxy), Option C (reuse MCP SA — proxy breakout is then MCP-equivalent)._

---

## Q4: Does `Ready` parse the investigation kubeconfig?

Today Ready is: Secret `cli-mcp-<name>-kubeconfig` exists with a non-empty `kubeconfig` key. It does **not** parse YAML. Kind e2e uses `--from-literal=kubeconfig=unused`. Dummy kubeconfig and kubernetes routes cannot be built from garbage, and the kubernetes injector cannot start. After Q1, this clause applies only when a kubernetes target exists, and the Secret is the **effective** name (`secretName` or the conventional default). Allowlist-only CRs skip this Secret.

### Option A: Parse token-only; invalid → not Ready, reason `KubeconfigInvalid`

Accept the same shape as claw `parseAndValidateKubeconfig`: inline tokens only; reject client certs, exec, auth-provider, basic auth, `tokenFile`, CA *file* paths. Missing object → `SecretsNotFound`. Empty key → `SecretKeysInvalid`. Parse/validate failure → `KubeconfigInvalid`. Do not apply dummy data / kubernetes routes / the kubeconfig volume on the proxy until parse succeeds. Still no separate `ProxyReady` condition (operator Q15: proxy children fold into `Ready`). Allowlist-only: skip this parse; still wait on CA + proxy Deployment + NPs.

- **Pro:** Fail closed. Status tells the admin the Secret is the problem. Proxy does not CrashLoop on `unused`.
- **Con:** Kind e2e and any “key present” fixture must ship a minimal token-only kubeconfig. Slightly stricter than today’s Ready.

**Decision:** Option A — token-only parse; `KubeconfigInvalid` when it fails; proxy children fold into the same `Ready`. Kind e2e fixture updates in the cutover PR. Session-safe rollout of the **proxy** (not a second Ready condition) is [Q11](credential-proxy-questions.md).

_Considered and rejected: Option B (key-non-empty; `Ready` while every `oc` is dead), Option C (separate `ProxyReady` — operator Q15; MCP cannot watch status)._

---

## Q5: How do the two Pod writers fail closed?

If sandbox pods are created before sandbox **egress** exists, they have unrestricted egress (the instance namespace has no default-deny). That window is the original bug.

The operator creates unassigned pool pods. MCP claims those or creates on demand. MCP **must not** watch the CR (operator Q9), so it cannot wait on `status.Ready`.

### Option A: Operator gates the pool; MCP gates on-demand create by GET of named children (flags, not CR)

Operator: do not create/replenish unassigned pods until the routes ConfigMap exists (dummy `kubeconfig` key when a kubernetes target exists), sandbox NP has Egress, proxy NP exists, and this proxy Service has a ready EndpointSlice address. Another Service’s slice must not open the gate. Overlay/hash rebuild waits on the same gate.

MCP: operator injects `--proxy-service` and `--proxy-ca-secret` always and `--dummy-kubeconfig-configmap` when a kubernetes target exists (conventional names; not spec fields). Before **on-demand create**, list EndpointSlices for that Service (`kubernetes.io/service-name`) and GET the sandbox NetworkPolicy (must include `policyTypes: Egress`). A ready address is `conditions.ready` nil or true with a non-empty address. Cache with a short TTL. **Claim** of an unassigned pod is allowed only if that gate passes too (do not claim a leftover pre-lock pool pod). Discovering an already-assigned session pod is unchanged.

MCP Role gains `list` on `endpointslices` (`discovery.k8s.io`) with no `resourceNames`, and `get` on `networkpolicies` with `resourceNames: [cli-mcp-<name>-sandbox]` (still **no** secret get). Slice object names are generated, so a named `get` cannot be scoped. The operator holds the same `endpointslices` `list` with no `resourceNames` on `manager-namespaced-role` and CSV `permissions`: one Role must read every instance, and that verb is what covers granting it on the MCP Role (same escalation check as `climcpinstances/mcp` on `manager-role` covering the client Role). `networkpolicies` `get` is already unnamed on the namespaced Role. Leave `climcpinstances/mcp` `get`/`create`/`delete` on ClusterRole `manager-role`. Do not add `endpoints` or `endpointslices` to the ClusterRole. Local `cmd/server` without those flags keeps today’s real kubeconfig mount and does not gate (runnable without the operator).

- **Pro:** Neither writer can open an unrestricted sandbox. MCP still does not watch the CR. Matches “flags from the operator.”
- **Con:** MCP Role is broader (list EndpointSlices in the namespace + get the sandbox NetworkPolicy). The label selector is what keeps another Service from opening the gate. First `bash` waits on proxy readiness (acceptable).

**Decision:** Option A — operator gates the pool; MCP lists this Service's EndpointSlices and GETs the sandbox NetworkPolicy before on-demand create and before claim. Assigned sessions are not gated (Q11). Local without proxy flags does not gate.

_Considered and rejected: Option B (withhold MCP Deployment only — running replicas still create open sandboxes), Option C (namespace default-deny — operator Q11), named core Endpoints `get` (`v1.Endpoints` is deprecated, and a generated EndpointSlice name cannot be `resourceNames`-scoped)._

---

## Q6: Require HTTP `Proxy-Authorization` in addition to NetworkPolicy?

kubectl honors `HTTPS_PROXY=http://user:pass@host:8080` and sends `Proxy-Authorization` on CONNECT. Proxy ingress NetworkPolicy is the primary control so only this instance’s sandbox pods can reach `:8080`. This would be a second factor if something in the **instance namespace** can spoof sandbox labels or if NP is mis-applied.

### Option A: NetworkPolicy only (v1)

- **Pro:** Matches claw-operator today (claw has **no** proxy-ingress NP and no proxy basic auth — we are already stricter on NP). Fewer secrets. Simple `HTTPS_PROXY`. A principal that can create pods in the CR namespace can **volume-mount the investigation kubeconfig Secret** (kubelet fetches it; the pod SA needs no `secrets get`) and talk to the API with **unrestricted egress** (throwaway pods are not selected by the sandbox NP). Proxy basic auth would not close that path.
- **Con:** Anyone who can run a pod with this instance’s sandbox labels can still use the proxy. That is the same create-pod compromise; extra CONNECT auth does not shrink it.

**Decision:** Option A — NetworkPolicy only. The instance namespace is the secret trust boundary (operator HOW). Proxy-Authorization does not help once an attacker can create pods.

_Considered and rejected: Option B (CONNECT basic auth — does not stop mounting the kubeconfig Secret; secret would be readable from sandbox bash)._

---

## Q7: How tight is proxy egress NetworkPolicy?

Sandbox egress is proxy + DNS only. Proxy egress must reach every API server listed in the investigation kubeconfig (often `:6443`, in-cluster `:443`). Claw’s kube path adds those ports to `0.0.0.0/0` and treats L7 as the real allowlist.

Vanilla NetworkPolicy matches **IP + port**, not DNS names. Parsing the kubeconfig (Q4) already gives L7 CONNECT hosts (`server` host:port). That does **not** give a hostname NP. Requiring a sibling `type: allowlist` on every kubernetes CR also does not: those `domains` are more L7 routes (Q1 union). First-party `oc` would have to duplicate kubeconfig servers into `domains[]` (drift). Allowlist-only CRs would be impossible.

Option B is “operator DNS-resolves those hostnames and writes `ipBlock`s.” It works in a lab with a stable A record. It fails when reconcile-time resolution ≠ CONNECT-time resolution (split-horizon, NLB/PrivateLink churn, extra A/AAAA, operator DNS view vs proxy pod DNS view). Stale CIDRs drop `oc` until the next resync (5m). Public allowlist hosts make that worse.

**What must stay:** sandbox **egress** NP (proxy + **cluster DNS only** — makes the proxy mandatory; not 53/5353 to `0.0.0.0/0`) and proxy **ingress** NP (`:8080` from this instance’s sandboxes only — Q6). This question is only **proxy pod egress**.

The allowlist that matters is L7: `MatchRoute` on the expanded `spec.proxy.targets` (kubeconfig `server` host:port and/or `allowlist` `domains`). Unknown CONNECT → 403 even if the CNI would allow the packet.

Option A is **generic Kubernetes** NetworkPolicy (`ipBlock`), not OpenShift-only. It only constrains **ports**, not destinations. Hardcoding `443`+`6443` also breaks an allowlist host on `:8443`.

### Option D: No proxy egress NetworkPolicy; L7 is the only host allowlist

Proxy pods have unrestricted egress (no namespace default-deny — operator Q11). CONNECT to anything not in this instance’s targets is still 403.

- **Pro:** One mechanism. No 443/6443 footgun. Investigation token exfil on 443 is already possible if the proxy process is compromised (kubeconfig is mounted there; Q6: create-pod can mount it too).
- **Con:** A buggy or compromised proxy can use any port, not only 443/6443.

**Decision:** Option D — no proxy egress NP. Host allowlist is `MatchRoute` on this CR’s targets. Keep sandbox egress + proxy ingress.

_Considered and rejected: Option A (wide 443/6443 `ipBlock` — does not enforce hostnames; custom ports break), A-only-when-kubernetes / D-otherwise (two NP shapes; adding allowlist drops rules; still no hostname backstop), Option B (reconcile-time DNS CIDRs), Option C (OpenShift EgressFirewall)._

---

## Q8: Allow CONNECT to raw IP addresses?

`MatchRoute` is hostname-based. `oc` uses the kubeconfig `server` URL (usually a hostname). An attacker in the sandbox can `curl -x $HTTPS_PROXY https://<api-ip>:6443` with a stolen token. Strip-then-inject still replaces `Authorization` when the host key matches; if the IP is **not** in the route map, inject fails closed but CONNECT might still tunnel. With Q7, there is no proxy egress NP to stop that packet.

### Option A: Reject CONNECT unless the host matches a route host:port

IPs only if that kubeconfig `server` or allowlist `domain` is already an IP. Do not DNS-resolve names and add the A/AAAA records. Operator stores every route as `host:port` (bare allowlist host → `:443`). Match is exact `host:port` only — not claw’s bare-host-matches-any-port, leading-dot suffix, or `*.` wildcards.

- **Pro:** No extra tunnel to “something on 6443.” Unknown host is 403.
- **Con:** A kubeconfig that mixes IP and hostname needs the IP as a cluster `server` URL if you want `curl` to the IP.

**Decision:** Option A — literal exact `host:port` only. No resolve-and-inject on IPs. No suffix/wildcard. Operator emits `host:port` (bare allowlist host → `:443`).

_Considered and rejected: Option B (map resolved IPs to the same token — DNS drift; shared LB IP may front more than the API; same class of bug as Q7-B)._

---

## Q9: Deny kube subresources at L7 (`exec` / `attach` / `portforward` / `proxy`)?

Investigation RBAC should already deny these. Bash can still *attempt* them. Claw kubernetes routes do not path-filter; `AllowedPaths` exists on the proxy for other injectors.

### Option B: Deny-list well-known mutating subresource path suffixes on kubernetes injector routes

Only `injector: kubernetes` (MITM’d kube API). Not `allowlist` / `none`. A mixed CR still filters kube routes and leaves allowlist paths alone.

Reject path suffixes `…/exec`, `…/attach`, `…/portforward`, `…/proxy` (impersonate is already header-stripped). Tests must keep `oc logs`, `oc get --watch`, `oc explain` allowed.

- **Pro:** Cheap belt given unconstrained bash. Survives a RoleBinding mistake. Does not break curl to an HTTP API that happens to contain `/exec`.
- **Con:** Path matching on the kube API is annoying (query strings, SPDY). False positives possible on kubernetes routes only.

**Decision:** Option B — denylist on kubernetes injector routes only. RBAC remains authoritative.

_Considered and rejected: Option A (RBAC only), applying the denylist to allowlist/`none` routes, applying it only when the CR has no allowlist target (mixed CRs still have kube routes that need the belt)._

---

## Q10: How much of the claw-operator proxy do we take?

Goal: own image, own code, no claw-operator release coupling. Claw is an **example** of MITM CONNECT + route injectors, not a package to vendor. Do not copy gateway/pathPrefix reverse-proxy, Slack rewrite, GCP token vending, oauth2, path_token, api_key, or `bearer`.

### Option A: Implement only the features this MCP proxy needs

MITM CONNECT **and** plaintext HTTP `OnRequest`, kubernetes injector, `none` (Q1 allowlist) **always MITM** (do not copy claw’s `none` → direct CONNECT tunnel — that skips strip), strip-then-inject, exact `host:port` (Q8), Q9 denylist on kubernetes routes, upstream TLS verify (never goproxy’s default `InsecureSkipVerify`), SIGTERM `Shutdown`. Read claw for how those pieces work; write `cmd/proxy` / `pkg/proxy` in this repo.

- **Pro:** No dead injectors. Matches v1 targets. We can differ from claw where our contract differs (proxy ingress NP, no proxy egress NP, `subPath`, dedicated SA).
- **Con:** Not a trivial diff against claw later. A later class that must *inject* a static bearer is a new injector then.

**Decision:** Option A — claw is inspiration. Implement `cli-mcp-proxy` for the features this design locked. Do not vendor `claw-operator/internal/proxy`.

_Considered and rejected: Option B (`bearer` in v1 — unused; allowlist is `none`), Option C (copy the claw package almost whole), treating this binary as a claw fork._

---

## Q11: How do in-place CR updates treat already-running sessions?

Sessions live in **sandbox pods**, not in the MCP process. The operator already must not delete pods with `session-id` except idle GC / CR delete. MCP rolling does not destroy those pods. The remaining blast radius is the **one shared proxy** (all sandboxes use the same Service DNS in `HTTPS_PROXY`) and **live ConfigMap/Secret mounts** (kubelet rewrites files in running pods unless mounted as `subPath`).

A second proxy generation (old Service for old sessions, new Service for new) needs extra DNS names, NetworkPolicies, and mixed CAs. Too much for v1. Waiting for idle timeout before applying an additive edit (`allowlist` domain) would stall a reasonable update for up to `idleTimeout`.

### Option A: Compatible-update contract; no generational proxy

**Do not disrupt assigned pods for reasonable edits. Apply immediately. Do not wait for drain.**

Mechanism:

- Never delete assigned sandbox pods on spec change (already locked).
- MCP Deployment rolls on its own; `/mcp` stays up when `spec.replicas ≥ 2` (existing MCP strategy). Sessions are not in those pods.
- Proxy Deployment: `replicas: 1`, **`maxUnavailable: 0`, `maxSurge: 1`**. Stamp **routes ConfigMap** RV + kubeconfig Secret RV + CA RV on the proxy pod template so kube starts a new proxy, waits Ready, then drops the old one. No inotify reload of kubeconfig/CA. Proxy process: `Shutdown` on SIGTERM (like claw), `terminationGracePeriodSeconds` covering that window, short `preStop` so ready addresses drop before the process dies. No sandbox retry wrapper; do not retry CONNECT **403**.
- Dummy kubeconfig and proxy CA on sandboxes: mount the data keys as **`subPath`** so assigned pods keep the files they started with. Unassigned pool hash-rebuilds when dummy/`ca.crt` **bytes** or proxy env change (not when the investigation Secret RV changes).
- NetworkPolicy is a live object: **add** rules only for v1 (sandbox egress to proxy). Do not tighten under running pods as part of a “safe” edit.

| Edit | Assigned session |
|---|---|
| Add `allowlist` domain / add kubeconfig cluster `server` (union) | bash + existing `oc` keep working; new hosts work after the new proxy is Ready. During surge (seconds) CONNECT to a **new** host may 403 if it hits the old proxy pod. |
| `spec.sandbox.image` / env / resources / `idleTimeout` / `warmPoolSize` | assigned keep the old pod; **new** sessions get the overlay. |
| `spec.replicas` | MCP rolls; sandboxes unchanged. |
| Token rotation in the same kubeconfig Secret (same `server` URLs) | proxy rolls; injects the new token; dummy `server` list unchanged → `oc` keeps working (investigation identity may change — that is the point of rotation). |
| Remove `kubernetes` target, change `secretName` to a different cluster, drop a `server`, narrow `domains`, break-glass CA delete | bash keeps running; `oc`/`curl` to removed hosts fail. Operator still does not kill the session. Wait for idle/DELETE. |

- **Pro:** Additive edits (the “add an allowlist” case) do not drain sessions and do not take `/mcp` down. Matches “not bulletproof for replacing kubernetes with a different type.”
- **Con:** Narrowing is immediately visible to assigned `oc`. During surge, two proxy configs coexist for a few seconds. `subPath` means assigned sandboxes will not pick up a rewritten dummy until the pod is replaced.

**Decision:** Option A — compatible-update contract. Apply immediately; no session drain; no generational proxy. Proxy drain on SIGTERM; no sandbox retry.

_Considered and rejected: Option B (wait for idle/no assigned sessions before rolling the proxy), Option C (generational proxy Service), sandbox `oc`/`curl` retry wrapper (403 is not a blip; clients do not share a retry policy), hot-reload of kubeconfig/CA (v1 still stamps and rolls; route-file reload is a later optional)._
