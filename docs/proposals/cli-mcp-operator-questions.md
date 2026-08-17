# CLI MCP Operator — design questions

**Status:** Decisions recorded
**Related:** [Design document](cli-mcp-operator-design.md)

This is the **HOW** (operator) for an **open-source Kubernetes operator**. First-party internal deploy is one catalog consumer; do not bake that environment into the API. Proxy WHAT stays in [credential-proxy-design.md](credential-proxy-design.md); proxy Q2–Q12 stay paused until this operator is implemented.

---

## Q1: Where does the operator live?

We are converting CLI MCP into an operator, then adding proxy children later. Layout now determines module boundaries, CD, and whether `cmd/server` stays a thin data plane. One repo should hold all components (controllers, MCP server, sandbox, later proxy) as multiple images — same shape as `claw-operator`.

### Option A: Same git repo, rename to `cli-mcp-operator`, multiple images

GitHub rename `cli-mcp-server` → `cli-mcp-operator` (issues/PRs/redirects kept). Go module path follows: `github.com/codeready-toolchain/cli-mcp-operator`. Add `cmd/operator` + `api/v1alpha1` beside existing `cmd/server` and `cmd/agent`. Separate images: `cli-mcp-operator`, `cli-mcp-server`, `cli-mcp-sandbox`, later `cli-mcp-proxy`. Image names do not have to match the repo (claw-operator already ships `claw-proxy`). Server must not import `internal/controller`.

- **Pro:** One PR surface for CRD + flag changes + later proxy. Matches claw-operator. No GitOps consumer yet, so rename is cheap. History stays one timeline.
- **Con:** `go.mod` gains controller-runtime/envtest (server binary still won’t link them if imports stay clean). One-time import-path churn. `pkg/session` lives under an `-operator` module (same as claw’s `internal/proxy`).

**Decision:** Option A — rename this repo in place to `cli-mcp-operator`; keep multiple images in one module. Do not create a second clone. Concrete tree, Makefile, and scaffold steps: [Repository layout](cli-mcp-operator-design.md#repository-layout-target) in the design (Kubebuilder go/v4 + `cmd/operator` building binary `manager`; types in-repo; do not copy host/member’s shared `api` repo).

_Considered and rejected: Option B (new `cli-mcp-operator` repo + move/archive old — extra remotes and broken links; nothing consumes this module yet), keep the name `cli-mcp-server` (repo would still say “server”), shorter name `cli-mcp` (vaguer in this org next to other MCP repos)._

---

## Q2: API group and Kind name?

The CR is the GitOps API for “one MCP instance / class.” It must not collide with `toolchain.dev.openshift.com` (UserSignup/Space) or `claw.sandbox.redhat.com`.

The operator should be a **generic Kubernetes operator** (installable on any cluster), not OpenShift-only like claw-operator. OpenShift-only features (serving certs, SCCs) may be enabled when the cluster is OpenShift. A dedicated public domain for the API group is not justified until there is real external community use; start on `redhat.com` and switch the group later if that appears.

### Option A: `cli-mcp.redhat.com` / `CliMcpInstance`

Group matches the instance label draft (`cli-mcp.redhat.com/instance`). Kind states “one instance,” which is what a later second class is. CRD lives in this repo (not `codeready-toolchain/api`).

- **Pro:** Stable, specific, not tied to one internal consumer. Kind matches proxy-Q1 language. No domain purchase until community demand is real.
- **Con:** Slightly long Kind. `redhat.com` without `sandbox.` unlike claw. A later group rename is a CRD conversion / bump if community appears.

**Decision:** Option A — `cli-mcp.redhat.com/v1alpha1`, Kind `CliMcpInstance`. Portable Kubernetes operator; OpenShift extras are optional detections, not requirements. Keep this group unless/until external adopters justify a new domain and group.

_Considered and rejected: Option B (`tarsy.redhat.com` / `CliMcpServer` — couples the CRD to one internal consumer and names the Kind after the Deployment), Option C (types in `codeready-toolchain/api` under `toolchain.dev.openshift.com` — that repo is an unrelated product API)._

---

## Q3: How is the operator installed?

Q2: this is a generic Kubernetes operator, not OpenShift-only. Claw-operator is OLM. Install artifacts in *this* repo are what any cluster uses.

OLM does **not** replace kustomize: the bundle is generated *from* `config/` (same as claw-operator ADR-0010). GitOps can subscribe to the catalog **or** apply kustomize. Helm can wrap the same manifests later.

### Option B: OLM OOTB like claw-operator; kustomize remains a supported install

Ship CatalogSource + bundle + catalog images and CD (`make bundle`, bundle/catalog push, PR `git diff` on `bundle/`) following claw-operator. `config/` kustomize is the source of truth. `make deploy` / `make dev-deploy` stay for local/dev and for clusters without OLM. Helm chart and extra docs are later, not v1.

- **Pro:** Same OOTB story as claw (OperatorHub-style install, channels, `relatedImages` for server/sandbox/later proxy). CI/CD already has a template in-tree next door. Does not block GitOps or Helm.
- **Con:** CSV, catalog image, scorecard, versioning — real tax. Vanilla Kubernetes users who do not run OLM use kustomize instead (documented escape hatch).

**Decision:** Option B — OLM is the default/OOTB install, including CI/CD like claw-operator. Kustomize stays supported (`config/`, `make deploy`). Helm is optional later documentation/chart, not a v1 deliverable. Supporting OLM does not contradict GitOps or Helm.

_Considered and rejected: Option A (kustomize-only / no OLM in v1 — user wants claw-parity OOTB including catalog CD), Option C (Helm as the primary portable install — can wrap the same manifests later if someone asks)._

---

## Q4: Which OLM install modes does the operator support?

Watch scope is an OLM **install mode**, not a hardcoded namespace. Children of a `CliMcpInstance` always live in **the CR’s namespace**.

Claw-operator’s CSV:

| Mode | Supported |
|---|---|
| OwnNamespace | yes |
| SingleNamespace | yes |
| MultiNamespace | no |
| AllNamespaces | no |

`WATCH_NAMESPACE` on the claw manager pod is the operator’s **own** namespace (downward API), used for operator-config lookup — not “only watch this one app namespace.” OLM RBAC (OperatorGroup `targetNamespaces`) is what limits which namespaces the operator can touch.

### Option A: Match claw — OwnNamespace + SingleNamespace

- **OwnNamespace:** operator Deployment and `CliMcpInstance` in the same namespace. Typical community install: one ns, CR there, MCP + sandbox pods there.
- **SingleNamespace:** operator in namespace A, reconcile CRs in one other namespace B (OperatorGroup `targetNamespaces: [B]`).
- Not MultiNamespace, not AllNamespaces.

- **Pro:** Same OLM story as claw. Tight RBAC. Portable: admin picks the namespace(s) at install time. No name baked into the binary.
- **Con:** One operator install cannot watch every namespace. A CR in a third namespace needs another Subscription or a mode we do not enable.

**Decision:** Option A — OwnNamespace + SingleNamespace, matching claw-operator. Children always live in the CR’s namespace. No product namespace in the binary.

_Considered and rejected: Option B (AllNamespaces — wide RBAC; claw disabled it), Option C (OwnNamespace only — drops SingleNamespace, which claw keeps for operator-in-one-ns / CR-in-another)._

---

## Q5: What does the admin own vs the operator vs the MCP process?

Wrong boundaries either put an ensure-loop for Deployments/NPs inside `cmd/server`, freeze derived objects in git, put the bash hot path through a Session CR, or leave multi-replica pool replenishment in every MCP replica.

Infrastructure (Deployment, Service, NPs) is rare and must reconverge. Session **create/claim** is synchronous with `bash`. Pool desired-count and idle janitor are not.

### Option A: Operator owns instance infrastructure + pool + idle GC; admin owns install and secrets; MCP owns the bash hot path

| Owner | Objects / jobs |
|---|---|
| **Admin / OLM** | CRD, operator Deployment/SA/RBAC, `CliMcpInstance` CR, investigation kubeconfig Secret `cli-mcp-<name>-kubeconfig`, TLS Secret `cli-mcp-<name>-tls` on generic Kubernetes, cluster-scoped RBAC (`system:auth-delegator`, MCP client `/mcp` access), OpenShift SCC / PSA as needed |
| **Operator** | MCP Deployment+Service, MCP SA, Role/RoleBinding (pods + secret create/delete), sandbox SA (Q12), NetworkPolicies (Q11), serving-cert annotation when on OpenShift, HMAC generate-once. **manager-role** namespaced secrets (HMAC, Ready, idle GC/finalizer) — OperatorGroup target namespace is the **secret trust boundary**. Warm pool, idle GC, teardown (Q10). Later: proxy children. |
| **MCP** | On `bash`: cache → discover → **always claim** a warm pod if one exists, else **create** on demand → wait Ready → HMAC `/exec`. Per-session auth Secret **create/delete only** (HMAC from file mount; no secret get/list/watch). `DELETE /sessions/{id}` deletes that session’s pod+secret. Does **not** replenish the pool or run idle GC. Claim is **not** gated on `--warm-pool-size` (Q9). |

No `Sandbox` / session CRD. Pool pods are ordinary Pods (no session-id). Claim stays a label patch (first writer wins). On-demand create stays in the MCP so an empty pool does not wait on the next operator reconcile for the first command.

- **Pro:** Request-synchronous work stays in the multi-replica MCP. Desired-count / janitor stay in the leader-elected operator (no replica pool stampede). Operator can later derive dummy kubeconfig/CA/routes. Admin does not hand-maintain the MCP Deployment. Tokens stay outside the operator. No second CRD.
- **Con:** Two writers of Pods (MCP create/claim, operator pool/GC) — contract must be label-based and idempotent. MCP still needs pod RBAC plus secret create/delete (not get/list) for the hot path.

**Decision:** Option A — this hybrid. Instance infrastructure + HMAC generate-once + warm pool + idle GC + instance teardown on the operator. MCP keeps claim / on-demand create / exec / explicit session delete. No session CRD. Investigation kubeconfig stays admin-provided.

_Considered and rejected: MCP owns pool and idle GC as well (replica stampede; janitor dies with the Deployment), `Sandbox` CRD (MCP apply CR, operator creates pods — extra hop, second API, HMAC on both sides, pool-as-CR awkward; same blast radius as `sandboxes create`), admin still owns the MCP Deployment (two owners; proxy would redo it), operator mints investigation tokens / cluster RBAC (identity provider), admin provides HMAC (internal MCP↔agent secret; ceremony with no admin-chosen value), copy as-built 2× idle age-drain of unassigned pool pods (MCP-replica overshoot leftover; operator trims surplus immediately), enqueue every last-activity patch or custom `AddAfter` per bash (hammers the instance reconciler; a later `requeueAfter` reread is enough), MCP secret get/list/watch (would read other CRs’ kubeconfigs and HMAC in the same namespace), RBAC `resourceNames` or label selectors for dynamic session Secrets (API cannot)._

---

## Q6: How much of today’s server flags belong on the CR spec?

`cmd/server` flags today: transport, address, stateless, namespace, sandbox-image, kubeconfig (MCP’s client), hmac-key-file, idle-timeout, warm-pool-size. In-cluster, transport/address/stateless/namespace are determined by the operator.

### Option A: Spec is the instance API — replicas, sandbox class (idle/pool/resources/image/env)

In-cluster-only flags are not on the spec (operator hardcodes HTTP/loopback/stateless). Namespace is `metadata.namespace`. Optional `spec.serverContainer` extras (resources, imagePullPolicy); not a free-form args array. `spec.sandbox.idleTimeout` and `warmPoolSize` are what the operator reconciles (Q5). No `spec.hmacKeySecretRef` — HMAC Secret is operator-owned.

- **Pro:** Reviewable, validated, matches claw. A second instance can differ. Pool/GC have a real API. Room to add proxy fields later without a junk drawer of CLI flags.
- **Con:** A new MCP flag needs a CRD field (or an escape hatch).

**Decision:** Option A — typed instance spec. No `spec.args` passthrough. Local `cmd/server` flags remain for running without the operator. Sandbox image lives at `spec.sandbox.image` (Q16), not a top-level `spec.sandboxImage`. No `spec.investigationKubeconfigSecretRef` — admin Secret name is `cli-mcp-<name>-kubeconfig` (same convention as TLS). This phase the operator mounts it on sandbox pods; proxy pass unmounts it from the sandbox and keeps the Secret for the proxy.

_Considered and rejected: Option B (opaque args/env — no validation, pool size is not a typed field), Option C (secret refs only — pool/idle would live on the operator Deployment or in code, so instances cannot differ), `spec.investigationKubeconfigSecretRef` (ceremony; TLS already uses a conventional name; two CRs get `cli-mcp-<name>-kubeconfig` without a spec knob)._

---

## Q7: How are MCP and sandbox images supplied?

All images (`cli-mcp-operator`, `cli-mcp-server`, `cli-mcp-sandbox`, later `cli-mcp-proxy`) are **ours** by default: same repo, same CD, Quay. The MCP server and operator stay that way. The **sandbox** image is the default class we ship; Q16 makes a different image a first-class instance setting, not a test-only pin. They are not an upstream workload like OpenClaw’s `spec.image` (the operator still supplies HMAC, labels, SA). Operator upgrades should pick up new **default** image tags automatically via OLM `relatedImages`. Do not bake `+kubebuilder:default` image tags into the CRD.

Operator-owned Deployments **fight** ImageStream triggers (the operator reverts the mutated image).

### Option A: OLM `relatedImages` → operator env defaults; sandbox class on the CR

CSV `relatedImages` stamp `RELATED_IMAGE_SERVER` / `RELATED_IMAGE_SANDBOX` / `RELATED_IMAGE_KUBE_RBAC_PROXY` (and later proxy) on the operator Deployment, same as claw’s `PROXY_IMAGE`. MCP server image is **always** `RELATED_IMAGE_SERVER` — no `spec.serverImage`. Local/dev points the operator at a locally built server image by overlaying that env (`make deploy`), so every CR follows. Empty `spec.sandbox.image` means `RELATED_IMAGE_SANDBOX`; set it for another class (Q16). Status records `resolvedSandboxImage` (spec vs default). MCP image is not on CR status. kube-rbac-proxy is a sidecar image, not an instance override field.

- **Pro:** Catalog/operator bump rolls every instance’s MCP together. No per-CR pin that survives upgrades. Sandbox class still differs per CR.
- **Con:** Cannot run two MCP *server* versions in one operator install (not needed; classes differ by sandbox, not `cmd/server`).

**Decision:** Option A — MCP image from operator env only. Not claw’s upstream-gateway `spec.image` + kubebuilder default.

**Revised by [Q16](#q16-how-does-the-crd-support-more-than-the-default-oc-sandbox):** `spec.sandbox.image` is a **first-class sandbox class** (BYO image that speaks the agent contract). Empty still means `RELATED_IMAGE_SANDBOX`. Do not add a closed enum of sandbox types or extra `RELATED_IMAGE_SANDBOX_*` keys to express classes.

**Revised:** drop `spec.serverImage`. Per-CR MCP pin blocks operator upgrades, and two CRs do not need different `cmd/server` images. Dev/test uses `RELATED_IMAGE_SERVER` on the operator Deployment.

_Considered and rejected: Option B (images always required on the CR — every bump edits every CR; operator upgrade would not move instances), Option C (ImageStream triggers on the MCP Deployment — operator and trigger both write the image), `spec.serverImage` optional pin (escape hatch that fights Q7’s upgrade story; local/dev already has operator-env overlay)._

---

## Q8: Instance identity — labels and child resource names?

NPs, pool, claim, and idle GC need a stable instance id. As-built sandbox pods use `tarsy.redhat.com/component` and `tarsy.redhat.com/session-id` in `pkg/session`. Nothing is in production; those keys can change in place (no migration).

Kubernetes already makes `metadata.name` immutable and DNS-1123. Child names `cli-mcp-<cr-name>` must still fit 63 characters.

### Option A: CR `metadata.name` is the instance id; labels under `cli-mcp.redhat.com`; children named `cli-mcp-<name>`

Labels: `cli-mcp.redhat.com/instance=<name>`, `cli-mcp.redhat.com/component` (`sandbox` | `server`), `cli-mcp.redhat.com/session-id` when assigned. Annotations: `cli-mcp.redhat.com/created-at`, `cli-mcp.redhat.com/last-activity`. Session manager and operator pool/GC select **component + instance** (and session-id when assigned). Drop `tarsy.redhat.com/*`. No client-pod label.

- **Pro:** One id. NP/pool selectors are obvious. A second CR cannot claim the first’s pods. Label domain matches the API group. No leftover internal domain in the open-source API.
- **Con:** Long CR names plus the `cli-mcp-` prefix can hit the 63-char Deployment name limit (use short CR names; `oc` is fine).

**Decision:** Option A — instance id is the CR name; `cli-mcp.redhat.com` labels; children `cli-mcp-<name>`. Replace as-built `tarsy.redhat.com` labels in `pkg/session` in the same change. No migration.

_Considered and rejected: Option B (encode instance only in a component label value — breaks current selectors and keeps a single overloaded key), Option C (random child names — worse UX; ownerRefs already GC)._

---

## Q9: Does the MCP process read the CR, or only flags the operator injects?

If `CliMcpInstance` spec changes, something has to pick that up. The MCP must **not** watch the CR.

### Option A: Operator patches the MCP Deployment; kube rolls the replicas

Operator translates spec → Deployment args/env/mounts. Kubernetes restarts MCP pods with the new flags. MCP never gets/watches `CliMcpInstance`. Local stdio stays flag-only.

Not every spec field needs a rollout: `spec.sandbox.warmPoolSize` / `idleTimeout` are consumed by the operator’s pool/GC (Q5). Fields that affect the MCP process (sandbox image/env/resources overlay, HMAC mount, SA, instance name, replicas) go on the Deployment. MCP **always** attempts claim then on-demand create; do not pass `--warm-pool-size` to gate claim (that would roll MCP on `0 ↔ N`).

- **Pro:** One control loop. Standard Deployment rollout. MCP SA needs no get-CR. Tests stay fake client-go.
- **Con:** Each new MCP knob is a flag + CR field + operator wiring. Secret *data* rotation (same Secret name) does not roll pods unless the operator stamps a hash/annotation (same as claw).

**Decision:** Option A — CR changes that affect the MCP go through the Deployment; kube rolls replicas. MCP does not watch the CR.

_Considered and rejected: Option B (MCP watches `CliMcpInstance` and self-configures — second reconciler in the data plane)._

---

## Q10: What happens to sandbox pods when the CR is deleted?

Delete CR must destroy that instance (including sandbox pods). Rolling the MCP Deployment must **not**. Background `ownerRef` GC alone can drop the CR while pods are still Terminating, so a same-name recreate can overlap.

### Option A: Finalizer quiesces MCP, then waits until session pods/secrets are gone; operator children keep `ownerRef` → CR

- **Finalizer:** while `deletionTimestamp` is set, do **not** re-ensure MCP `replicas`. Scale the MCP Deployment to 0, **wait until this instance’s `component=server` pods are gone**, then list instance-labeled sandbox pods and session Secrets, delete, **wait until gone**, then remove the finalizer. The CR name stays taken until session cleanup finishes.
- **`ownerRef` → CR:** MCP Deployment, Service, NPs, SAs, Role/RoleBinding, HMAC Secret, warm-pool pods. After the finalizer drops, Kubernetes GCs these. Do not wait for those objects in the finalizer.
- **MCP** does not set `ownerRef` on on-demand session pods and does not get the CR. Idle GC (Q5) is the same label list in steady state.

- **Pro:** Name reuse cannot overlap Terminating session pods. MCP is not still creating sessions while the finalizer waits. MCP Deployment rollout does not kill sessions. Standard `ownerRef` for operator children. MCP stays flag-only (Q9).
- **Con:** A stuck finalizer if the operator is down (don’t remove it unless you intend to orphan). Operator must not SSA-delete assigned session pods as “unexpected children.”

**Decision:** Option A — finalizer is the teardown guarantee (quiesce MCP, then session objects); `ownerRef` on operator-created children; MCP session pods are not owned by the Deployment.

_Considered and rejected: ownerRef-only on session pods (background GC + same-name overlap), MCP ownerRef to the CR as the sole mechanism (still need the finalizer), orphan / idle-only (after CR delete nothing reconciles those pods), delete session pods while MCP is still Running (MCP recreates; a create after the list goes empty orphans a pod with no ownerRef), wait for every ownerRef child in the finalizer (ownerRef GC after the finalizer drops)._

---

## Q11: Which NetworkPolicies does this phase create?

Proxy NPs (sandbox egress-to-proxy-only, proxy ingress/egress) are the next design. This phase still needs the as-built ingress rules or `/exec` is open inside the namespace.

### Option A: Ingress only — MCP `:8443` from configured clients; sandbox `:8090` from this instance’s MCP pods. No sandbox egress policy

- **Pro:** Same security as the current design docs. Instance label on sandbox ingress so a later second MCP cannot hit these agents. Does not fake a token-replay fix.
- **Con:** Sandbox egress stays open. Explicitly accepted until proxy.

**Decision:** Option A, amended — operator-owned **sandbox** ingress NP only (`:8090` from this instance’s `component=server` pods). That selector is labels, not identity; `/assign` stays once-unauthenticated. The OperatorGroup target namespace is the trust boundary (same as secrets). No MCP `:8443` NetworkPolicy and no client pod label; kube-rbac-proxy is the MCP front door (clients often cannot set labels). Egress lock waits for proxy.

_Considered and rejected: egress NP / EgressFirewall to kube API IPs now (stopgap that must be removed when the proxy exists; OpenShift API IPs are painful), admin-applied NPs (cannot select MCP-created sandbox pods without the same labels/selectors the operator should own), MCP ingress NP requiring `cli-mcp.redhat.com/mcp-client` on callers (impractical; kube-rbac-proxy already authenticates `/mcp`), HMAC or mTLS on `/assign` or a VAP that only the MCP SA may set `component=server` (namespace is the trust boundary; identity-aware assign is proxy work)._

---

## Q12: Sandbox pod identity in this phase?

Nothing is in production. The MCP will not be used until the proxy exists. This phase still has to be **testable**: `oc` in a session pod must work so we can verify the operator (pool, claim, HMAC `/exec`, idle GC, CR delete) before the proxy pass.

Dummy kubeconfig without a proxy makes `oc` fail and blocks that testing. Keeping the as-built investigation SA + automount-true is not “preserving a product”; it is carrying a footgun into the first operator tests.

### Option B: Dedicated sandbox SA + `automountServiceAccountToken: false` now; still mount the real kubeconfig

- Operator creates `cli-mcp-<name>-sandbox` with **no RoleBindings** (OpenShift needs an SA; this is the proxy-ready identity).
- MCP sets that SA on sandbox pods and `automountServiceAccountToken: false`.
- Admin-provided investigation kubeconfig Secret stays mounted so `oc` works in tests.
- Proxy pass later swaps the mount for dummy kubeconfig + `HTTPS_PROXY`; SA and automount already match.

- **Pro:** Operator tests still cover real `oc`. Pod identity is already the proxy shape. Accidental automount of an investigation SA never exists in this operator.
- **Con:** Sandbox disk still holds real tokens until proxy (accepted; this phase is not the isolation ship).

**Decision:** Option B — proxy-ready pod identity now; real kubeconfig mount until proxy so the operator is testable.

_Considered and rejected: keep investigation SA and default automount (no product to preserve; extra projected token on every test pod), dummy kubeconfig now (breaks `oc`, cannot test the operator before proxy)._

---

## Q13: Who owns MCP client authentication (kube-rbac-proxy extras)?

In-cluster MCP is typically: kube-rbac-proxy sidecar, TLS on the Service, `system:auth-delegator` on the MCP SA, a **client** SA + token, and RBAC allowing that client to call `/mcp`. The MCP client uses that token. Loopback-only bind in `cmd/server` exists for this sidecar.

### Option A: Operator owns sidecar + Service TLS (serving-cert when on OpenShift); admin owns cluster-scoped auth-delegator, client SA, `/mcp` ClusterRole/Binding

Operator always injects kube-rbac-proxy (not optional). Admin (or `config/samples` in tests) creates the client SA and points the MCP client at it.

- **Pro:** Operator stays mostly namespaced. ClusterRole for `/mcp` is a cluster API grant — belongs to the admin. Sidecar is an implementation detail of the MCP Deployment the operator already owns.
- **Con:** Standing up an instance is “CR + a few cluster RBAC YAMLs,” not CR-only.

**Decision:** Option A — kube-rbac-proxy is a fixed part of the MCP Deployment (`RELATED_IMAGE_KUBE_RBAC_PROXY`). Cluster-scoped client auth stays with the admin; ship sample YAMLs so operator tests can call `/mcp`. `--allow-paths`: `/mcp`, `/metrics`, `/live`, `/health`, `/sessions`. TLS Secret `cli-mcp-<name>-tls`: serving-cert on OpenShift, admin-provided on generic Kubernetes.

_Considered and rejected: operator creates auth-delegator, client SA, and `/mcp` ClusterRole (operator ClusterRole too wide; cluster-scoped GC on CR delete is awkward), no kube-rbac-proxy (no TLS + SA-token front door; breaks loopback-only bind)._

---

## Q14: Put `spec.proxy` on the CR now (disabled), or omit until the proxy design?

### Option A: Omit. Extension point is “same CR, new fields + children later”

- **Pro:** Does not freeze injector types, route lists, or CA knobs before proxy Q2–Q12. CRD additive changes are normal for v1alpha1.
- **Con:** First CRD bump in the proxy PR (expected).

**Decision:** Option A — document the extension point; do not stub `spec.proxy` on this CRD.

_Considered and rejected: `spec.proxy.enabled: false` and empty structs now (speculative API; proxy questions will reshape the field; dead validation)._

---

## Q15: What does `status.Ready` mean in this phase?

`Ready` is how we notice that the instance did **not** come up — including a warm pool that cannot initialize. The operator owns the pool (Q5), so pool health belongs on this condition. Assigned session pods (on-demand / claimed) are not part of Ready. A claim is success, not an outage: `Ready` must not flicker every time the pool is replenished.

### Decision

`Ready` requires **all** of:

1. **Required Secrets exist with the conventional non-empty keys** (no kubeconfig/YAML/cert parse). `cli-mcp-<name>-kubeconfig` data `kubeconfig`; HMAC `cli-mcp-<name>-hmac` data `key`; on generic Kubernetes also `cli-mcp-<name>-tls` data `tls.crt` and `tls.key`. Missing object → not Ready, reason `SecretsNotFound`. Missing or empty required key → not Ready, reason `SecretKeysInvalid`. HMAC is operator-created (part of (2)); generate-once does **not** overwrite a pre-existing Secret, so an empty/wrong-key HMAC stays `SecretKeysInvalid`. Extra Secrets referenced only from `spec.sandbox.env` are **not** this check. OpenShift serving-cert TLS is platform-filled; Deployment Available still waits on the sidecar mount.
2. **Every operator-managed namespaced child is present and matches spec** (SAs, Role/RoleBinding, Service, NetworkPolicies, sandbox SA, HMAC Secret).
3. **MCP is ready to receive requests:** Deployment Available — desired replicas ready, including the kube-rbac-proxy sidecar (readiness probes on both containers).
4. **Warm pool, if `warmPoolSize > 0`:**
   - **First Ready** (and after `warmPoolSize` increases): wait until there are `warmPoolSize` unassigned **Ready** pods.
   - **After that:** a claim/replenish dip does **not** clear `Ready` unless a pool pod is Failed / ImagePullBackOff / CrashLoopBackOff, or the pool is still short of desired past a **replenish deadline** (operator constant, not a spec field). Decreasing `warmPoolSize` does not wait.
   - Always publish `status.warmPoolReady` / `status.warmPoolDesired`. Optional condition `WarmPoolReady` is the strict count (may flap); aggregate `Ready` does not flap on claim.

`warmPoolSize: 0` skips (4). Later proxy children fold into the same `Ready`. No separate `ProxyReady` until that pass needs it.

_Considered and rejected: Ready = MCP Deployment + Secrets only (stuck pool looks Ready), Ready tracks unassigned Ready count every second (claim looks like an outage), Ready after only one pool pod, no status beyond observed generation, parse the kubeconfig or TLS certs (key presence is enough), a separate `InvalidSecret` condition (same gate as Ready; use reason `SecretKeysInvalid`)._

---

## Q16: How does the CRD support more than the default oc sandbox?

The sample CR is named `oc` and Q7’s `RELATED_IMAGE_SANDBOX` is the agent+oc image we ship. That must not freeze the API as “one kubectl sandbox.” A second class (curl, aws-cli, a user’s image) is another `CliMcpInstance` in the same namespace (Q8: CR name is the instance id). The MCP tool is still `bash`; what varies is the **pod image and its config**.

The constraint: session pods must speak the **agent HTTP contract** (`/health`, `/exec`, `/assign` on the agent port, HMAC). A custom image typically `FROM` our sandbox image or copies `cmd/agent`. This is not a generic Job/Pod operator.

### Option A: Nested `spec.sandbox` is the class — BYO `image`, additive knobs, operator-owned base

- `spec.sandbox.image` omitted → `RELATED_IMAGE_SANDBOX` (the class we ship). Set → that class’s image. First-class, not a test escape hatch (revises Q7).
- v1 mergeable knobs on `spec.sandbox`: `resources`, `env` (`[]corev1.EnvVar`, including `valueFrom` for extra Secrets), `imagePullPolicy`. Empty `resources` → as-built DefaultConfig requests/limits, not BestEffort. Extra `valueFrom` Secrets are not Ready gates.
- Operator-owned (not in spec, not overridable): sandbox SA, automount, session token env (assigned/on-demand; pool via `/assign`), instance/component labels, probes, agent port, kubeconfig mount **in this phase** (Q12), today’s non-root / drop-caps security context. Reserved env names win if the user sets them: `KUBECONFIG`, `HOME`, `SANDBOX_AUTH_TOKEN`.
- No `spec.sandbox.type` enum. No full `PodTemplateSpec` (would fight HMAC, labels, SA, two Pod writers).
- Later additive fields on the same object: extra volumes/mounts, `imagePullSecrets`, args, `securityContext` override, optional kubeconfig mount, agent port. Same rule as Q14: do not stub them empty now.
- Investigation kubeconfig Secret `cli-mcp-<name>-kubeconfig` (key `kubeconfig`) is **required this phase** so operator tests can run `oc` (mounted on sandbox pods). No spec ref — conventional name, same as TLS. Proxy pass **unmounts from sandbox** and keeps the Secret for the proxy. A future class that does not need kube is an additive change (skip mount if absent), not a new Kind.
- Pool recreate hash includes the resolved sandbox spec (image + env + resources), not only the image tag. Assigned sessions keep the old spec until they end.
- Operator maps CR spec → `SandboxConfig` in-process for pool pods. MCP gets the same overlay as Deployment flags (Q9). Overlay change rolls MCP. No overlay ConfigMap. `pkg/session` must not import `api/v1alpha1`.

- **Pro:** Second CR `curl` / `aws` is a different image+env, same operator. Users bring an image without a code change. CRD stays typed. Room to add mounts later without a rewrite.
- **Con:** Custom images must include a compatible agent (and `curl` for the exec readiness probe, as-built). Env merge cannot override operator-owned names.

**Decision:** Option A — one CR = one sandbox class; `spec.sandbox.image` + additive `env`/`resources`/`imagePullPolicy`; shipped image is the default, not the only type. Extend `spec.sandbox` later; do not add a type enum or a pod template.

_Considered and rejected: closed `type: oc|curl` + `RELATED_IMAGE_SANDBOX_*` (not universal; every new class is an operator release), `spec.sandbox.template` PodSpec (operator cannot own HMAC/labels/SA safely), keeping sandbox image as tests-only (Q7 as originally written), a separate SandboxClass CRD (second API for one nested object), overlay ConfigMap or a versioned SandboxConfig wire format (Deployment flags; pool maps spec in-process)._
