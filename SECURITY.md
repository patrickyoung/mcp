# Security

Discovery is untrusted description, not authority. `mcpbox make` writes no
callable capabilities; only an explicit `mcpbox admit` does so. Generated
wrappers verify the current descriptor digest before a tool, prompt, or exact
resource request.

Do not place credentials in server argv. Supply them through an
operator-owned environment, keychain, or credential helper. Capability
folders intentionally contain endpoint identity, discovery data, schemas,
descriptions, and the server runtime `PATH`; inspect them before sharing.

A capability directory limits names available to Ply. It is not adversarial
OS confinement. Use Cage, a container, VM, separate OS identity, and explicit
network policy when a worker or MCP server must not inherit ambient host
authority.

Resource URIs remain data and are never treated as local filesystem paths.
This release does not fetch icons, remote assets, or MCP App resources except
through an explicit admitted MCP request.

Please report vulnerabilities privately to the repository owner rather than
opening a public exploit issue.
