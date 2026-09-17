# Compatibility

[Getting started](getting-started.md) · [Protocol](protocol.md) · [README](../README.md)

## Versions and prerequisites

| Component | Scope |
| --- | --- |
| Go | 1.25 or newer; CI covers 1.25.x and Orka's current 1.27.x |
| Official A2A Go SDK | `github.com/a2aproject/a2a-go/v2` pinned to **v2.5.0** |
| A2A wire protocol | **1.0**, JSON-RPC, the [documented text-only subset](protocol.md); not full conformance |
| Orka gateway contract | `orka.gateway.v1` ingress/callback and raw operator event/delivery JSON, exercised against Orka main at **[`f97724a7`](https://github.com/orka-agents/orka/commit/f97724a772b6c09be39749ddf01795bd0a9ce571)** |
| Runtime-backed Agents | Use Orka's existing gateway path; provision a working runtime-backed Agent and matching route |
| Native `type: ai` Tasks for Agents without a runtime | Supported by merged [PR #564](https://github.com/orka-agents/orka/pull/564), using the Session ownership fix merged in [#565](https://github.com/orka-agents/orka/pull/565); both are included in the tested Orka revision |

This is **not** a blanket compatibility claim for every Orka main revision or
published release. Build Orka from the tested revision for this native path; no
published release or image tag is asserted to contain these changes. Recheck the
contract when upgrading Orka; its [release status](https://github.com/orka-agents/orka/blob/main/website/docs/reference/release-status.md)
explains the difference between release snapshots and main.

Orka must have the gateway CRDs, persistent gateway ledger, and authenticated
operator HTTP API enabled. The adapter's small HTTP projections are in
[`gateway.go`](../gateway.go); it does not import the controller's internal
packages. No controller or native-dispatch changes are included in this repository.
No copied prerequisite or unmerged native-dispatch patch is needed at the tested
revision. Installing this adapter alone does not upgrade an older Orka deployment.

## Where this adapter came from

This repository carries the separately deployed adapter first developed as
`examples/a2a-gateway` in a local Orka worktree, not merged into Orka. [Orka #557](https://github.com/orka-agents/orka/issues/557)
records the original proposal and placement discussion; the adapter now lives in
`orka-agents/orka-gateway-a2a`.

The runtime Go files and gateway/RBAC/config examples are byte-identical to the
original module at local commit **`fccc1759`**. Six long client-test lines were
wrapped to meet the standalone lint rules; the test's parsed syntax tree is
unchanged. The module path is now `github.com/orka-agents/orka-gateway-a2a`.
Documentation, build targets and CI are maintained here. The original commit was
not published upstream; these checkpoints are not adapter release tags.

## Verification and limits

The tests exercise real SDK discovery/JSON-RPC/client calls against a TLS adapter,
with a controlled HTTP fixture **only at the external Orka boundary**. They cover
auth/version checks, retry envelopes, ownership, final/error output correlations,
blocking/cancellation, callbacks/restarts/rotation, bounds, and redirect refusal.
They do not prove real model execution, Kubernetes RBAC deployment, image builds,
or general A2A conformance. The separate CI container job checks an image build,
not a deployed cluster. CI uses no cluster credentials or cloud model calls.

### Merged-code real execution

On **2026-09-17**, standalone adapter main **[`1fbd64ce`](https://github.com/orka-agents/orka-gateway-a2a/commit/1fbd64ce760ac8ae97a9867661475da3794e91f1)**
was built and deployed with unmodified upstream Orka **`f97724a7`** in a disposable
kind cluster (Kubernetes **v1.32.2**). The model was local CPU Ollama **0.11.8**,
**`qwen2.5:3b`** (model ID `357c53fb659c`). Normal authenticated admission, persistent
Orka storage, TLS callbacks and the adapter's projected ServiceAccount were enabled.

The official SDK client made **27 calls including polling**, producing **two real
native Tasks**, not 27 executions:

- Discovery and an immediate send followed by `GetTask` returned the first model
  answer, `cedar comet`.
- A fresh message ID with the same A2A context asked for the remembered phrase;
  blocking send returned `cedar comet` from a different Task sharing the Session.
- Identical retries before and after completion retained the original Task identity.
- Both answers matched the actual Task UIDs and final durable deliveries; callbacks
  were acknowledged through the real outbox.
- `GetTask` returned the same completed result after restarting only the adapter.
- Seven verified-TLS/authentication probes and ten least-read RBAC checks passed.
  Running controller, worker, model and adapter image identities matched the
  digest-pinned builds.

Both Tasks succeeded. The cluster, local registry and port-forwards were removed.
No paid model API or cloud resources were used; local compute/storage costs were
not measured. This proves the native text path at those revisions, **not** the
runtime-provider matrix, full A2A conformance, production readiness or enforcement
of NetworkPolicies by kind's default CNI. Private credentials and operational logs
are not distributed with this repository.

### Historical real execution

Before the standalone import, the original module at **`fccc1759`** was exercised
against real Orka native-AI code at **`bc7a8953`**, including the separately credited
local Session ownership prerequisite **`4c13635c`**. That run produced two real
native Tasks using Ollama **`qwen2.5:3b`**, checked same-Session memory across the two
messages, and checked deduplication and adapter restart behavior against Orka's
durable ledger.

That is evidence for the **original module and those exact local Orka revisions**.
It is separate from the merged-code run above and does not establish general A2A
conformance or production readiness. Future live checks must likewise use the real
ledger and actual Task output, not fixture completion. Private credentials, transcripts,
and operational logs are not distributed with this repository.

### Operational limits

- **One caller trust domain.** Sharing the bearer shares access to every owned
  Task in that domain; there is no per-user/OIDC/federated authorization.
- **Pull delivery.** `delivered` means a result is available through `GetTask`,
  not that an external caller consumed it. Callback acknowledgement needs a real,
  correlated durable record, so arbitrary synthetic generic conformance-probe
  deliveries are intentionally rejected.
- **Orka-owned retention.** Full-record removal makes `GetTask` unavailable;
  tombstones, deduplication expiry, and cleanup stay in Orka. The public Task ID
  includes the admission timestamp to prevent an old ID reading a later admission.
- **A narrow protocol subset.** No streaming, subscriptions, list, same-task
  continuation, file/data input, artifact download, or successful cancellation.
  Push configuration and extended cards are unsupported too. See the full
  [supported-call and input rules](protocol.md#supported-calls).
- **Deployment is operator-owned.** Provide Secrets, TLS trust, the Agent, a
  selector-backed Service, least-privilege API access, and boundary rate/concurrency
  limits. No adapter PVC, rate scheduler, or second execution store is supplied.
