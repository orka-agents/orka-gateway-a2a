# Compatibility

[Getting started](getting-started.md) · [Protocol](protocol.md) · [README](../README.md)

## Versions and prerequisites

| Component | Scope |
| --- | --- |
| Go | 1.25 or newer; CI covers 1.25.x and Orka's current 1.27.x |
| Official A2A Go SDK | `github.com/a2aproject/a2a-go/v2` pinned to **v2.5.0** |
| A2A wire protocol | **1.0**, JSON-RPC, the [documented text-only subset](protocol.md); not full conformance |
| Orka gateway contract | `orka.gateway.v1` ingress/callback and raw operator event/delivery JSON, checked against Orka main at **`7a279c40`** |
| Runtime-backed Agents | Use Orka's existing gateway path; provision a working runtime-backed Agent and matching route |
| Native `type: ai` Agents without a runtime | Require the open native gateway [PR #564](https://github.com/orka-agents/orka/pull/564) and the gateway/Session ownership prerequisite subset tracked in [#313](https://github.com/orka-agents/orka/issues/313) |

This is **not** a blanket compatibility claim for every Orka main revision or the
latest published Orka release, v0.1.3. The Orka contract baseline above and the
historical native-AI run below are different checkpoints. Recheck the contract
when upgrading Orka; its [release status](https://github.com/orka-agents/orka/blob/main/website/docs/reference/release-status.md)
explains the difference between release snapshots and main.

Orka must have the gateway CRDs, persistent gateway ledger, and authenticated
operator HTTP API enabled. The adapter's small HTTP projections are in
[`gateway.go`](../gateway.go); it does not import the controller's internal
packages. No controller or native-dispatch changes are included in this repository.
PR #564 and #313 remain open; installing this adapter does not make native gateway
dispatch available on unmodified main. Only the relevant Session ownership subset
of #313 was used in the historical run, not that entire workstream.

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

### Historical real execution

Before the standalone import, the original module at **`fccc1759`** was exercised
against real Orka native-AI code at **`bc7a8953`**, including the separately credited
local Session ownership prerequisite **`4c13635c`**. That run produced two real
native Tasks using Ollama **`qwen2.5:3b`**, checked same-Session memory across the two
messages, and checked deduplication and adapter restart behavior against Orka's
durable ledger.

That is evidence for the **original module and those exact local Orka revisions**.
It is not new live-cluster proof for a binary built from this repository, does not
mean the open upstream prerequisites have merged, and does not establish general
A2A conformance or production readiness. A new live check must use the real ledger
and actual Task output, not fixture completion. Private credentials, transcripts,
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
