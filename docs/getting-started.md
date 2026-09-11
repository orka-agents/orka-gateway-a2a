# Getting started

[Protocol](protocol.md) · [Compatibility](compatibility.md) · [README](../README.md)

## Before you start

Use Go 1.25 or newer and a working Orka installation with gateway CRDs, a persistent
gateway ledger, and the authenticated operator HTTP API enabled. Provision a
runtime-backed Agent named `assistant` in `demo`, or change the matching names in
the configuration and manifests. Native `type: ai` Agents need additional upstream
work; check [Compatibility](compatibility.md) before choosing that path.

[`config.example.json`](../config.example.json), [`gateway.yaml`](../gateway.yaml),
and [`rbac.yaml`](../rbac.yaml) are small wiring examples, not a turnkey cluster
installation. They contain **no secret values**. You supply the namespace, Agent,
Secrets, adapter Deployment, selector-backed Service, and network policy. Read
Orka's [gateway operations guide](https://github.com/orka-agents/orka/blob/main/website/docs/operations/gateways.md)
alongside this guide.

One adapter deployment serves one configured caller trust domain. **Everyone
sharing its caller token can read every Task in that domain.** Do not use it as a
per-user or cross-tenant authorization boundary.

## Configuration and credentials

Copy the example config to a non-secret file outside the repository and edit it
for your deployment. Review the card's name, description, version, and skills as
public information. Never copy the Agent's system prompt, tools, or complete CR
into the card. The endpoint, protocol version, text modes, security declaration,
and capabilities come from adapter code, not untrusted request data. The example
card version is operator-declared metadata, not an adapter release number.

Create four distinct credentials using your normal Secret provisioning process:

| Mounted file | Purpose |
| --- | --- |
| `clientTokenFile` | Opaque external caller bearer; high-entropy random token |
| `inboundTokenFile` | Gateway-bound credential for adapter → Orka admission |
| `outboundTokenFile` | Different Gateway-bound credential for Orka → adapter probes/receipt |
| `readTokenFile` | Rotating projected operator ServiceAccount token for Orka HTTP reads |

Gateway Secrets `a2a-inbound` and `a2a-outbound` each use key `token`. They need
annotation `gateway.orka.ai/gateway-name: a2a` on **both** Secrets (the same label
may also be set), plus respectively label `gateway.orka.ai/inbound-auth: "true"`
and `gateway.orka.ai/outbound-auth: "true"`. The outbound Secret additionally needs
annotation `gateway.orka.ai/adapter-endpoint: https://a2a-gateway.demo.svc:8443`.

Never put credentials in config, card metadata, command arguments, logs, image
layers, or raw Secret manifests. Token files are reopened on requests for rotation;
use projected directory mounts, not `subPath` mounts that freeze Secret updates.
The process requires TLS certificate/key files and loads the serving certificate
at startup. **Restart the adapter to rotate its serving certificate.**

## RBAC and route setup

Use [`rbac.yaml`](../rbac.yaml) for ServiceAccount `demo/a2a-gateway`. It grants only
`get` on the named Agent, Gateway, and Binding, and `get` on the namespace's virtual
`gatewayevents`/`gatewaydeliveries` API resources. Event IDs are created at runtime,
so those two grants cannot have a fixed `resourceNames` list; Orka additionally
requires `get` on the associated named Gateway. No list, mutation, Secret, Task,
Session, cluster-admin, or wildcard rights are needed.

If your installation enables transaction-token/context-token scopes, provision its
corresponding Agent/gateway-read scopes separately; this adapter does not exchange
TxTokens. The Agent read endpoint returns a CR to this trusted adapter, but only
metadata is decoded. This admin-read privilege is necessary to verify card routing;
it must not be handed to external clients.

Install the GatewayClass/Gateway/Binding from [`gateway.yaml`](../gateway.yaml)
before starting the adapter. Startup verifies real `metadata`/UIDs, `gatewayRef`,
`agentRef`, exact match account/context/sender, a singleton sender allowlist, and
`thread-sender` mode. `/readyz` and every send (including a duplicate retry)
revalidate that route. A replacement UID requires adapter restart. **Do not gate
startup on Gateway readiness:** its connection probe requires this adapter to be
listening first. Configure only one applicable binding; Orka owns overlap/priority
resolution and rejects ambiguous matches.

## DNS, TLS, and networking

`publicURL` is the externally reachable **HTTPS origin**, without a path, trailing
slash, query, or fragment. Discovery is at the root well-known path and the
advertised JSON-RPC endpoint is `<publicURL>/a2a`. Do not place discovery behind an
unexplained URL prefix. Keep TLS to the adapter, not only at an ingress.

The Gateway `serviceRef` certificate must cover `a2a-gateway.demo.svc`; public
clients need a certificate valid for their public hostname. Use a certificate
covering both when sharing this listener. Trust private CAs in both Orka and
clients; see the [gateway operations guide](https://github.com/orka-agents/orka/blob/main/website/docs/operations/gateways.md).
There is no `InsecureSkipVerify` option.

Set the administrator-controlled `orkaURL` to the actual operator API origin.
The example uses `http://orka-api.orka-system.svc:8080`, the standard Orka API
Service. Its private in-cluster HTTP is an **internal trust boundary carrying
bearer credentials**, not an endpoint to expose publicly. Restrict it by network
policy or supply verified HTTPS. Upstream redirects are refused.

Deploy with the ServiceAccount, read-only config/credential/TLS mounts, and a
selector-backed Service `a2a-gateway` exposing TLS port 8443. No adapter PVC is
needed. Run as non-root with a read-only root filesystem and dropped capabilities.
Set resource limits and network policy/rate/concurrency limits at your deployment
boundary. There is no multi-tenant admission scheduler or adaptive rate limiter
here. Request and response bounds are listed in [Protocol](protocol.md#request-boundaries).

## Build and run

From the repository root:

```bash
make test
make vet
make build
./bin/orka-gateway-a2a -config /path/to/non-secret-config.json
```

Before starting, provision all files named in the config, make them readable by
the process identity, and ensure the configured Orka API is reachable. The process
will not start with missing credentials, TLS files, or an unverifiable route.
Use `./bin/orka-gateway-a2a -help` to see the config flag.

### Run in a container

Build your own image; no published adapter image is assumed:

```bash
make docker-build
```

For a Docker run with already-provisioned files, prepare this directory layout
**outside the repository**:

```text
prepared-adapter-files/
  config/config.json
  tls/tls.crt
  tls/tls.key
  client/token
  inbound/token
  outbound/token
  read/token
```

Keep the in-container file paths from `config.example.json`. Set its `publicURL`
and `orkaURL` to origins reachable in your environment, with matching TLS names
and Gateway wiring. Mount directories rather than individual token files so the
process sees credential rotations. Ensure the container's non-root identity can
read the files without making credentials world-readable.

```bash
CONFIG_DIR=/absolute/path/to/prepared-adapter-files

docker run --rm --name orka-gateway-a2a \
  --read-only --cap-drop ALL --security-opt no-new-privileges:true \
  --publish 8443:8443 \
  --mount "type=bind,src=${CONFIG_DIR}/config,dst=/etc/a2a,readonly" \
  --mount "type=bind,src=${CONFIG_DIR}/tls,dst=/etc/a2a-tls,readonly" \
  --mount "type=bind,src=${CONFIG_DIR}/client,dst=/etc/a2a-client,readonly" \
  --mount "type=bind,src=${CONFIG_DIR}/inbound,dst=/etc/a2a-inbound,readonly" \
  --mount "type=bind,src=${CONFIG_DIR}/outbound,dst=/etc/a2a-outbound,readonly" \
  --mount "type=bind,src=${CONFIG_DIR}/read,dst=/var/run/secrets/kubernetes.io/serviceaccount,readonly" \
  orka-gateway-a2a:local
```

This demonstrates runtime file mounts, **not** a Kubernetes deployment. A local
Docker container does not normally resolve in-cluster `*.svc` names or satisfy
the Gateway's selector-backed Service by itself. Arrange connectivity in both
directions before running it, or run the image in your operator-owned Deployment.
For Kubernetes, use a projected, rotating ServiceAccount token rather than a
static copy; bind-mounting a token locally does not arrange its renewal.

## Use the client

Use your public URL and an external caller token **file**, never the Gateway or
operator tokens. For a private CA add `-ca-file /path/to/ca.crt`; do not disable
TLS verification.

```bash
# Blocking by default: prints the returned Task as JSON.
./bin/a2a-client -url https://agent.example.com \
  -token-file /path/to/client-token -message-id example-question-1 \
  -text 'What is 17 times 23?'

# Return a submitted/working Task, then poll its complete returned A2A Task ID.
./bin/a2a-client -url https://agent.example.com \
  -token-file /path/to/client-token -message-id example-question-2 \
  -return-immediately -text 'Explain the answer briefly.'
./bin/a2a-client -url https://agent.example.com \
  -token-file /path/to/client-token -task-id '<returned-a2a-task-id>'
```

To send two messages into the same `thread-sender` Session, use a fresh message ID
for each and the same `-context-id`. These create two Orka Tasks, not a continuation
of one A2A Task:

```bash
./bin/a2a-client -url https://agent.example.com \
  -token-file /path/to/client-token -context-id example-conversation \
  -message-id example-conversation-1 -text 'Remember that the project is called Harbour.'
./bin/a2a-client -url https://agent.example.com \
  -token-file /path/to/client-token -context-id example-conversation \
  -message-id example-conversation-2 -text 'What is the project called?'
```

Actual memory behavior belongs to the configured Agent and Orka execution path.
The adapter does not reconstruct A2A history. The client resolves the SDK card
first without credentials, then requires its interface to match the configured
HTTPS origin and `/a2a` path before sending a token. It refuses redirects and
bounds responses.

Preserve message ID, text, and optional context exactly on retry, including after
a blocking timeout. Never recycle a message ID for a different request. Work is
not canceled by client timeout or disconnect; use the complete returned Task ID
for polling, not a bare Orka event ID. See [Protocol](protocol.md) for admission,
route-change, and retention behavior.
