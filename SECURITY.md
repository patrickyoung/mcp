# Security

Discovery is untrusted description, not authority. `mcpbox make` writes no
callable capabilities; only an explicit `mcpbox admit` does so. Generated
wrappers verify the current descriptor digest before a tool, prompt, exact
resource, or resource-template request. Template admission authorizes the
reviewed template; a concrete URI must match it before a read is sent.

Do not place credentials in server argv. For HTTP, open an operator-owned
header file on `mcp -header-fd` or `mcp-legacy -header-fd`, or point an
admitted wrapper at one with `MCP_HEADERS`. Capability folders intentionally
contain no credential. They do contain endpoint identity, discovery data,
schemas, descriptions, and the server runtime `PATH`; inspect them before
sharing.

`mcpserve -http` is a protocol listener, not an identity provider or TLS
terminator. Bind it locally or place it behind an operator-owned reverse proxy
that enforces TLS, OAuth, audience, and network policy. Do not expose the bare
listener to an untrusted network.

A capability directory limits names available to Ply. It is not adversarial
OS confinement. Use Cage, a container, VM, separate OS identity, and explicit
network policy when a worker or MCP server must not inherit ambient host
authority.

Resource URIs remain data and are never treated as local filesystem paths.
This release does not fetch icons, remote assets, or MCP App resources except
through an explicit admitted MCP request.

Legacy support is an explicit executable choice. `mcp` never downgrades into a
stateful session, and `mcp-legacy` never probes a server with modern discovery.
The compatibility process still creates a fresh connection per invocation,
never reconnects or retries, and records the effect boundary identically.

An `mcpserve` dispatcher is executable authority. It receives untrusted MCP
parameters and may inherit the service account's environment and filesystem.
Use a narrow account and Cage/container/VM confinement where the dispatcher is
not fully trusted. Its stdout and event fd are bounded and parsed; its stderr
is diagnostic data and must not be interpreted as protocol.

Please report vulnerabilities privately to the repository owner rather than
opening a public exploit issue.
