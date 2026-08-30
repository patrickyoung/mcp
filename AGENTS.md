# Development guidance

`mcp` is the protocol edge between MCP and ordinary Unix programs. It is a
sibling of Bench, Ply, Tend, and Agent; none of those programs know MCP exists.

- One `mcp request` is one self-contained request to one explicit endpoint.
- stdin is one bounded JSON object, stdout is the exact result, stderr is
  diagnostics, and exit status is the outcome.
- Never retry a request automatically. Once the request write succeeds, a
  missing trustworthy terminal response is exit 125.
- Modern stateless MCP is the default and only implicit mode. Legacy sessions
  require a separate, explicit compatibility process.
- Use the official Go SDK for protocol behavior. Keep Unix process lifetime,
  process groups, byte limits, and effect accounting in this repository.
- Server stderr remains stderr. Progress and subscriptions use explicit JSONL
  streams; they never create a second transcript or hidden durable store.
- `mcpbox` compiles discovery into a folder. Generation is staged and atomic;
  discovery grants nothing; admission binds a descriptor digest; an
  unadmitted or changed capability must not remain executable.
- Effectful admission is explicit. `admit ... actions` generates connectors
  for the standalone Action filter; it never trusts MCP annotations to bypass
  operator policy or May.
- Generated programs pin the endpoint, capability name, and reviewed digest.
  They accept exact JSON on stdin and do not contain credentials.
- No daemon, endpoint registry, credential database, hidden cache, automatic
  reconnect, prompt installer, resource index, task poller, or model callback.
- Run `go test ./...` and `go test -race ./...` before reporting success.
