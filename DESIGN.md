# Design

## Sentence

`mcp` sends one self-contained MCP request to one explicit server and prints
the exact result.

`mcp-legacy` exposes the same filter contract behind an explicit legacy
session lifecycle; it is never selected by fallback from `mcp`.

`mcpbox` compiles one server's discovery result into a folder whose executable
entries are the exact capabilities an operator admitted.

`mcpserve` publishes declared MCP capabilities while running each behavior as
one ordinary Unix filter process.

## Boundary

MCP remains a wire protocol at the edge. Inside a Unix worker it becomes
programs, JSON streams, files, explicit remote handles, signals, and exit
status. Ply receives a `PATH`; Context receives evidence; Ask owns the event
log; May owns approval; Cage owns confinement; Tend owns local durability.
None of them imports this code or learns MCP.

The protocol implementation comes from
`github.com/modelcontextprotocol/go-sdk`. This repository adds:

- exact stdio argv, stateless Streamable HTTP, and process-group lifetime;
- an explicit legacy initialization mode isolated in a companion executable;
- bounded JSON stdin and bounded server output;
- result-byte capture before typed SDK decoding can discard unknown fields;
- an irreversible sent boundary and exit 125 after uncertain effects;
- explicit JSONL progress and subscription streams;
- descriptor-digest admission and runtime verification;
- RFC 6570 template admission and expansion checks;
- staged, atomic folder generation;
- a reverse adapter from MCP requests to bounded Unix filter processes.

## Capability folder

```text
server.mcp/
  endpoint.json
  discover.json
  runtime.json
  catalog/
    tools.jsonl
    tools.pages.jsonl
    prompts.jsonl
    prompts.pages.jsonl
    resources.jsonl
    resources.pages.jsonl
    templates.jsonl
    templates.pages.jsonl
  admit/
    tools.tsv
    prompts.tsv
    resources.tsv
    templates.tsv
  tools/
  prompts/
  resources/
  bin/
    read
    read-template
```

The per-item catalogues are convenient Unix streams. The page catalogues
retain exact list-level metadata, cache scope, TTLs, and cursors. Endpoint and
runtime files contain no credentials. `PATH` is retained because an absolute
launcher such as `npx` may still require an interpreter or child programs; it
is part of the endpoint digest and is restored inside generated wrappers.

Each admission line is `identity<TAB>sha256`. The digest covers:

```text
kind + canonical endpoint + discovered server identity + canonical descriptor
```

For tools and prompts the identity is `name`; for resources it is `uri`; for
templates it is `uriTemplate`. A name surviving while its schema,
annotations, description, endpoint, or server identity changes does not
retain authority.

## Effect boundary

The transport wrapper observes successful SDK connection writes. Discovery
and runtime descriptor checks are observations. The requested method is armed
separately; its successful call write irreversibly sets `sent`.

A matching unique JSON-RPC result is trustworthy. A matching JSON-RPC error is
a complete peer negative. Once `sent` is true, loss or ambiguity before that
terminal record is exit 125 and is never retried. The whole server process
group is closed on every path, including descendants surviving the direct
server process.

## MCP surfaces

The modern and legacy executables share request dispatch, exact result capture,
effect classification, and admission verification. Their lifecycle policy is
the deliberate seam: `mcp` requires `2026-07-28` discovery, while
`mcp-legacy` forces the SDK-owned `initialize` path and accepts the SDK's four
pre-2026 revisions. Its exact initialize result supplies capabilities and
server identity to the existing digest model. Deprecated HTTP+SSE and legacy
long-lived notification subscriptions are outside this one-request adapter.

- Tools have generated, digest-checked executable wrappers.
- Prompts have separate operator-invoked, digest-checked filters.
- Exact resources use a generated allowlisting reader.
- Resource templates have a whole-template admission. The reader verifies the
  descriptor, then requires the concrete URI to match its RFC 6570 template.
- Completion and extension Tasks use ordinary `mcp request` calls.
- MRTR input requests are returned as data with exit 75; a caller explicitly
  retries with `requestState` and `inputResponses`.
- Subscriptions are a real `subscriptions/listen` request exposed as one
  foreground JSONL stream and never reconnect.
- Streamable HTTP refuses redirects and SDK retries. Credentials enter through
  an explicit header descriptor, never argv or a generated folder.
- Images, audio, embedded resources, MCP Apps payloads, structured content,
  and unknown `_meta` remain in the exact result JSON.
- Deprecated roots, sampling, and logging receive no ambient handler. A future
  compatibility program may provide explicitly selected handlers.

## Reverse edge

`mcpserve` separates descriptions from behavior. A JSON manifest supplies the
server identity, capability descriptors, and extension method names. The
official SDK owns `server/discover`, list pagination, subscriptions, request
routing, metadata, cancellation, and stdio or stateless HTTP framing.

For a call, one dispatcher process receives:

```text
argv    configured dispatcher argv + MCP method
stdin   one params JSON object
stdout  one result object
stderr  unchanged diagnostics
fd 3    zero or more progress/log notification envelopes as JSONL
```

Exit 0 accepts the result. Exit 1 accepts only a JSON-RPC error object. Every
other exit is an execution failure. Input, output, event-line size, duration,
and the entire process group are bounded or operator-controlled. The manifest
can expose Tools, Prompts, Resources, Resource Templates, Completion, and
arbitrary extension methods such as Tasks without teaching the dispatcher
about MCP framing or connection state.

Tasks remain an extension without becoming an in-process framework. The
manifest advertises the extension and names its three lifecycle methods;
`mcpserve` validates capability negotiation, HTTP `Mcp-Name` routing,
polymorphic creation results, task state, and acknowledgement envelopes. The
dispatcher owns durable creation and lookup, so Tend, SQLite, files, or a
remote service can implement the state machine independently.

A manifest is immutable for one server process and consequently advertises no
list-change or legacy resource-subscription capability. An attempted false
claim is rejected at startup. Replacing the process is the Unix configuration
change boundary.

## Not present

No daemon, connection pool, registry, endpoint config search, OAuth flow or
token store, TLS termination, hidden retry, hidden MRTR callback, task
database, task poller, prompt installer, resource index, Apps renderer, model
client, approval path, confinement path, or second trace. HTTP authorization
is composed with an operator-owned reverse proxy or explicit header producer.

## Next slices

1. Add `mcp-unpack` for digest-named materialization of binary content.
2. Add an optional Registry search filter that writes endpoint proposals and
   never installs or executes them.
