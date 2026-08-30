#!/bin/sh
set -eu

prefix=${HOME}/.local

usage() {
	cat <<'EOF'
usage: ./install.sh [-prefix DIR]

Build mcp, mcp-legacy, mcpbox, and mcpserve from this checkout and install them under DIR/bin.
The default prefix is $HOME/.local.
EOF
}

while test "$#" -gt 0; do
	case $1 in
		-prefix)
			shift
			test "$#" -gt 0 || { usage >&2; exit 2; }
			prefix=$1
			;;
		-h|--help)
			usage
			exit 0
			;;
		*)
			usage >&2
			exit 2
			;;
	esac
	shift
done

command -v go >/dev/null 2>&1 || {
	echo 'install.sh: Go 1.26 or newer is required' >&2
	exit 2
}

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/mcp-install.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

(cd "$here" && go test ./...)
(cd "$here" && go build -trimpath -o "$tmp/mcp" ./cmd/mcp)
(cd "$here" && go build -trimpath -o "$tmp/mcp-legacy" ./cmd/mcp-legacy)
(cd "$here" && go build -trimpath -o "$tmp/mcpbox" ./cmd/mcpbox)
(cd "$here" && go build -trimpath -o "$tmp/mcpserve" ./cmd/mcpserve)

mkdir -p "$prefix/bin"
install -m 0755 "$tmp/mcp" "$prefix/bin/mcp"
install -m 0755 "$tmp/mcp-legacy" "$prefix/bin/mcp-legacy"
install -m 0755 "$tmp/mcpbox" "$prefix/bin/mcpbox"
install -m 0755 "$tmp/mcpserve" "$prefix/bin/mcpserve"

echo "installed mcp, mcp-legacy, mcpbox, and mcpserve in $prefix/bin" >&2
