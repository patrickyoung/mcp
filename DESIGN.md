# Design

## Sentence

`mcp` sends one self-contained MCP request to one explicit server and prints
the exact result.

`mcpbox` compiles one server's discovery result into a folder whose executable
entries are the exact capabilities an operator admitted.

## Boundary

MCP remains a wire protocol at the edge. Inside a Unix worker it becomes
programs, JSON streams, files, explicit remote handles, signals, and exit
status. Ply receives a `PATH`; Context receives evidence; Ask owns the event
log; May owns approval; Cage owns confinement; Tend owns local durability.
None of them imports this code or learns MCP.

The protocol implementation comes from
`github.com/modelcontextprotocol/go-sdk`. This repository adds:

- exact stdio argv and process-group lifetime;
- bounded JSON stdin and bounded server output;
- result-byte capture before typed SDK decoding can discard unknown fields;
- an irreversible sent boundary and exit 125 after uncertain effects;
- explicit JSONL progress and subscription streams;
- descriptor-digest admission and runtime verification;
- staged, atomic folder generation.

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

- Tools have generated, digest-checked executable wrappers.
- Prompts have separate operator-invoked, digest-checked filters.
- Exact resources use a generated allowlisting reader.
- Resource templates remain visible catalogues until a safe expansion and
  admission contract is specified.
- Completion and extension Tasks use ordinary `mcp request` calls.
- MRTR input requests are returned as data with exit 75; a caller explicitly
  retries with `requestState` and `inputResponses`.
- Subscriptions are one foreground JSONL stream and never reconnect.
- Images, audio, embedded resources, MCP Apps payloads, structured content,
  and unknown `_meta` remain in the exact result JSON.
- Deprecated roots, sampling, and logging receive no ambient handler. A future
  compatibility program may provide explicitly selected handlers.

## Not present

No daemon, connection pool, registry, endpoint config search, OAuth token
store, hidden retry, hidden MRTR callback, task database, task poller, prompt
installer, resource index, Apps renderer, model client, approval path,
confinement path, or second trace.

## Next slices

1. Add Streamable HTTP with the SDK and an operator-owned credential helper.
2. Add safe template URI expansion with descriptor-bound admission.
3. Add `mcp-unpack` for digest-named materialization of binary content.
4. Add the foreground `mcpserve` CGI-like adapter for exporting explicit Unix
   capability directories.
5. Add an optional Registry search filter that writes endpoint proposals and
   never installs or executes them.
