# mcp

MCP at the edge; ordinary Unix inside.

`mcp` sends one self-contained request to one explicit MCP server. `mcpbox`
turns that server's discovered capabilities into a reviewable folder of Unix
programs and catalogues. Neither is a daemon, framework, SDK for applications,
credential store, workflow engine, or model host.

The first release deliberately supports modern stateless MCP `2026-07-28`
over stdio. It uses the official Go SDK for the protocol and owns only the
Unix process contract around it.

## Install

Requires Go 1.26 or newer.

```sh
go install ./cmd/mcp ./cmd/mcpbox
```

## One request

The server command always follows `--`, so it remains exact argv rather than
shell text. From this checkout, the included official-SDK server makes a
complete local smoke test:

```sh
printf '%s\n' '{"name":"hello","arguments":{"name":"Unix"}}' |
  mcp request tools/call -- go run ./examples/hello-server
```

Use `mcp discover -- SERVER [ARG ...]` to inspect any other modern server.

stdin is empty or one bounded JSON object. stdout is the exact MCP result.
The server's stderr remains stderr. `-event-fd 3` writes progress JSONL to an
explicit descriptor. `-timeout 30s`, `-max-input`, and `-max-output` put
operator-owned bounds around a call. There is no default timeout and there is
never an automatic retry.

Every standard request surface is available through `mcp request`, including:

```text
tools/list                   tools/call
prompts/list                 prompts/get
resources/list               resources/templates/list
resources/read               completion/complete
io.modelcontextprotocol/tasks/get
io.modelcontextprotocol/tasks/update
io.modelcontextprotocol/tasks/cancel
```

Extension methods are passed through the official SDK's custom-method seam.
Lifecycle methods are not: use `mcp discover`; legacy `initialize` sessions
are intentionally rejected.

An `input_required` result is printed intact and exits 75. Fulfil the returned
`inputRequests`, retain the opaque `requestState`, then make a new explicit
request containing `inputResponses`. `mcp` never calls Ask, May, a terminal,
or a model on the server's behalf.

## Compile a capability folder

Discovery grants nothing:

```sh
mcpbox make hello.mcp -- go run ./examples/hello-server
mcpbox show hello.mcp
mcpbox tools hello.mcp
```

`mcpbox tools` prints a TSV catalogue containing the tool name, its descriptor
digest, and its synopsis. Admission is a separate literal action:

```sh
mcpbox admit hello.mcp tools hello
printf '%s\n' '{"name":"Pike"}' | hello.mcp/tools/hello
```

An admitted tool wrapper pins the endpoint, tool name, and reviewed digest.
Immediately before `tools/call`, `mcp` lists the current descriptor through the
same connection and refuses changed or missing tools without sending the
call.

Prompts and resources retain MCP's different control model:

```sh
mcpbox prompts server.mcp
mcpbox admit server.mcp prompts review-pr
printf '%s\n' '{"tone":"brief"}' | server.mcp/prompts/review-pr

mcpbox resources server.mcp
mcpbox admit server.mcp resources 'repo://README.md'
server.mcp/bin/read 'repo://README.md'
```

The generated resource reader accepts only exact admitted URIs. Resource
templates are catalogued but are not automatically expanded or admitted.

Refresh into a new folder and use the ordinary system diff:

```sh
mcpbox make server.next.mcp -- SERVER [ARG ...]
mcpbox diff server.mcp server.next.mcp
```

Generation uses a private staging directory and one final rename. A failed or
fresh generation contains no callable capability. Catalogue changes never
inherit admission by name.

## Streaming

```sh
mcp listen -- SERVER [ARG ...]
```

`listen` emits list-change notifications as JSON Lines until interrupted or
the stream breaks. It never reconnects. A stream that disappears is a failure,
not an invented continuous history.

## Outcomes

| Exit | Meaning |
| ---: | --- |
| 0 | complete positive result |
| 1 | complete peer or application negative, including `isError` |
| 2 | local usage, validation, discovery, or pre-transmission failure |
| 75 | valid but unfinished: `input_required` or a nonterminal Task |
| 125 | the requested action was transmitted but no unique trustworthy terminal result arrived |
| 130 | interrupted before transmission |

After the requested call's write succeeds, EOF, timeout, malformed framing,
a wrong response ID, a duplicate terminal response, or an output-limit breach
is 125. No hint or retry policy weakens that boundary. This composes directly
with Tend's `unknown` state.

See [DESIGN.md](DESIGN.md) for boundaries, file formats, and remaining work.
