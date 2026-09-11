# Protocol

[Getting started](getting-started.md) · [Compatibility](compatibility.md) · [README](../README.md)

The adapter uses the official A2A Go SDK for Agent Card discovery, A2A types,
JSON-RPC encoding, and its client. A custom `a2asrv.RequestHandler` reads Orka's
existing event/delivery ledger. There is **no SDK default executor, task store,
second queue, database, or history store**. Orka owns execution, retries, retention,
and cleanup.

## Supported calls

| Surface | Behavior |
| --- | --- |
| `GET /.well-known/agent-card.json` | Public, explicitly reviewed metadata; SDK-generated card |
| `POST /a2a`, `SendMessage` | One user text part, creating one new task per message ID |
| `POST /a2a`, `GetTask` | Authorized projection of the durable event and final delivery |
| `CancelTask` | Ownership check, then `TaskNotCancelable`; no gateway queued-event cancel seam |
| List / same-task continuation / streaming / subscriptions | Explicit unsupported errors |
| Push configurations / extended card | `PushNotificationNotSupported` / `UnsupportedOperation` |

Authenticated protocol calls require `A2A-Version: 1.0`. A missing header denotes
legacy 0.3, **not** implicit 1.0; missing/other versions are rejected. There is no
REST/gRPC binding, legacy conversion, extension negotiation, or full-conformance
claim. Unsupported capability booleans are false (omitted by the SDK's JSON encoder).

Inputs are one `ROLE_USER` message with a nonempty stable `messageId` and one text
part (`text/plain` or omitted media type). File/URL/data parts, reference tasks,
`taskId` continuation, metadata, extensions, nonempty tenants, and push configuration
are rejected without admission. Nonnegative `historyLength` is accepted; history
is omitted rather than reconstructed. `acceptedOutputModes`, when supplied, must
include `text/plain`.

`returnImmediately: true` returns after durable admission and one authorized read;
it does not wait for execution. The default blocks, polling once per second for
up to 30 seconds including admission. Timeout/disconnect never cancels real work
or invents a failed Task. Retry the **identical message ID and payload**, or use
`GetTask` once you have the returned Task ID. Every SendMessage, **including retries**,
rechecks the configured route. After route drift a retry can fail even while
GetTask still reads an owned, retained result under the pinned object identities.

## Caller identity and Sessions

One deployment represents one configured caller trust domain, account, routing
context, sender, GatewayBinding, and Agent. The opaque client bearer token selects
that domain; clients cannot supply Orka namespace/account/sender identities.
**Everyone sharing this token can read every task in this adapter's domain.** This
is not per-user authorization, OIDC, federation, or a multi-tenant service.

The gateway `externalEventId` is a digest of the configured route/sender and A2A
`messageId`, not the text. No optional ingress timestamps or metadata are sent.
A supplied bounded A2A `contextId` is preserved as the gateway `threadId`; without
one, a stable opaque context is derived from the same identity. Retrying an initial
message therefore produces the same immutable envelope. Altered text/context with
the same message ID is a gateway conflict, not a new task. A new message ID with
the same context creates another Task in the `thread-sender` Session, not a
continuation of the previous A2A Task. Configure only one applicable binding; Orka
still owns overlap/priority resolution and rejects ambiguous matches.

## Task IDs and retention

The A2A Task ID fences Orka's **server-generated event ID and durable admission
instant**, not a caller ID or Task CR name. Its exact versioned encoding is:

```text
a2a1.<base64url-no-padding(UTF-8 event.id)>.<base64url-no-padding(UTF-8 canonical-createdAt)>
canonical-createdAt = event.createdAt converted to UTC and formatted with Go time.RFC3339Nano
```

RFC3339Nano retains nanosecond precision, removes trailing fractional zeroes (and
the decimal point when the fraction is zero), and ends in `Z`. Each GetTask/CancelTask
decodes the event segment, reads the owned row, then compares the **entire canonical
ID exactly**, including `createdAt`. Missing/zero admission timestamps fail closed.
No adapter store or new core field is needed: core also derives Task creation
identity from this persisted admission instant. Retry/restart is stable for the
same retained admission. After full-row and tombstone expiry Orka can reuse an
event ID, but that new admission gets a different public Task ID; the old ID returns
`TaskNotFound`, never a new execution. Bare event IDs are intentionally not accepted
as A2A Task IDs, including IDs returned by earlier versions of the original example.

For operator correlation only, split the returned ID on `.` into exactly three
segments and base64url-decode segments two and three (restore `=` padding if your
decoder requires it). Use the decoded event ID with Orka's gateway-event API; keep
the **complete returned ID** for A2A GetTask and client `-task-id` calls.

Orka's retention window bounds GetTask/retry availability. Once full records are
removed, GetTask returns `TaskNotFound`; deduplication tombstones and their later
expiry remain Orka-owned. The adapter neither prolongs retention nor recreates
lost history. See [gateway operations](https://github.com/orka-agents/orka/blob/main/website/docs/operations/gateways.md)
for durability, expiry, and cleanup guarantees.

## Ownership and results

Reads check namespace, Gateway, Binding, and Agent (including pinned object
UIDs), account, context, and sender. Orka's read API additionally fences the current
namespace/Gateway incarnation. Foreign/missing events return `TaskNotFound`.
Pre-binding denial rows lack the required Binding/Agent ownership and are likewise
not exposed; such SendMessage attempts can return `TaskNotFound` after admission.

| Orka event | A2A state |
| --- | --- |
| Accepted / Queued | Submitted |
| Dispatching / TaskCreated | Working |
| Rejected (owned) | Rejected |
| Expired / DeadLettered (owned) | Failed |
| Completed + correlated `kind: error` delivery | Failed with sanitized delivery text |
| Completed + correlated nonempty `kind: final` delivery and Task identity | Completed with one text Artifact, stable ID `final` |
| Completed without valid final data / unknown state | Server error; never fabricated success |

Event/delivery namespace, Gateway, Binding, event, Task, Session, account, context,
thread, and reply-target correlations must agree before any delivery text is exposed.
Final/completed-result references remain strict. For `kind: error`, an Expired event
may omit only an unverified Task reference; a Rejected/DeadLettered denial may omit
both Task and Session references. These are core's failure-delivery shapes, not
permission to accept conflicting nonempty references. Callback receipts must match
the exact durable references, including omissions, and pass the same terminal-result
projection as GetTask before acknowledgement.

Raw CRs, prompts, operator `stateMessage`/`lastError`, and upstream error bodies are
not returned. The text Artifact is A2A encoding, **not** Orka artifact publication
or download support.

## Pull delivery and callbacks

Delivery *kind* represents the execution result. Outbox transport state (including
retry/Failed/DeadLettered/Expired) does not erase an existing final result available
to the pull client. The callback validates against the durable ledger and returns
a deterministic `providerMessageId`; repeated receipt or adapter restart needs no
receipt store. Here `delivered` means **available through GetTask**, not evidence
that an external client consumed the answer. Invented, unowned, or mismatched
callbacks are rejected; a missing ledger is a retryable HTTP 503. This intentionally
does not accept arbitrary synthetic deliveries from the generic conformance probe.

`GET /v1/health`, `GET /v1/capabilities`, and `POST /v1/deliveries` all require the
outbound Gateway bearer. `/readyz` reveals only readiness and no credentials.

## Request boundaries

Authentication precedes body parsing. Inbound bodies are bounded to 256 KiB, text
to 64 KiB, identity to 256 bytes, and upstream reads to 1 MiB with ten-second request
timeouts. Malformed JSON, trailing documents, malformed UTF-8, and unpaired UTF-16
escapes are rejected before admission. Valid surrogate pairs and literal
backslash-u text are accepted.

Invalid bodies on `/a2a` return JSON-RPC parse error `-32700` with `id: null`;
callbacks retain separate HTTP errors. Byte limits still use HTTP 413 and
authentication failures HTTP 401. Upstream redirects are refused. See
[Getting started](getting-started.md#dns-tls-and-networking) for the internal
operator API trust boundary and TLS requirements.
