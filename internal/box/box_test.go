package box

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patrickyoung/mcp/internal/admit"
)

func TestMakeGrantsNothingUntilAdmit(t *testing.T) {
	root := t.TempDir()
	fake := filepath.Join(root, "mcp")
	writeFakeMCP(t, fake, false)
	target := filepath.Join(root, "example.mcp")
	endpoint := admit.Endpoint{Type: "stdio", Command: "/bin/echo", Args: []string{"server"}, Path: os.Getenv("PATH")}
	if err := Make(context.Background(), target, endpoint, Config{MCP: fake}); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(filepath.Join(target, "tools"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("fresh toolbox contains callable entries: %v", entries)
	}
	admission, err := os.ReadFile(filepath.Join(target, "admit", "tools.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	if len(admission) != 0 {
		t.Fatalf("fresh admission is not empty: %q", admission)
	}

	var listing bytes.Buffer
	if err := List(&listing, target, "tools"); err != nil {
		t.Fatal(err)
	}
	firstLine := strings.Split(strings.TrimSpace(listing.String()), "\n")[0]
	fields := strings.Split(firstLine, "\t")
	if len(fields) != 3 || fields[0] != "echo" || len(fields[1]) != 64 {
		t.Fatalf("listing = %q", listing.String())
	}

	if err := Admit(target, "tools", []string{"echo"}, Config{MCP: fake}); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(target, "tools", "echo")
	info, err := os.Stat(tool)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("admitted tool is not executable: %v", info.Mode())
	}
	wrapper, err := os.ReadFile(tool)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wrapper, []byte("mcpbox")) {
		t.Fatal("admitted tool retains a runtime dependency on mcpbox")
	}
	if !bytes.Contains(wrapper, []byte(fake)) {
		t.Fatal("admitted tool does not invoke the configured mcp filter")
	}
	cmd := exec.Command(tool)
	cmd.Stdin = strings.NewReader(`{"hello":"world"}`)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != `{"content":[],"structuredContent":{"hello":"world"}}` {
		t.Fatalf("wrapper output = %s", out)
	}

	if err := Revoke(target, "tools", []string{"echo"}, Config{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tool); !os.IsNotExist(err) {
		t.Fatalf("revoked tool remains: %v", err)
	}
	if err := Admit(target, "tools", []string{"remote-schema"}, Config{MCP: fake}); err == nil {
		t.Fatal("external schema reference was admitted")
	}
	if _, err := os.Stat(filepath.Join(target, "tools", "remote-schema")); !os.IsNotExist(err) {
		t.Fatalf("refused external schema tool became callable: %v", err)
	}

	if err := Admit(target, "prompts", []string{"review"}, Config{MCP: fake}); err != nil {
		t.Fatal(err)
	}
	prompt := filepath.Join(target, "prompts", "review")
	cmd = exec.Command(prompt)
	cmd.Stdin = strings.NewReader(`{"tone":"brief"}`)
	if out, err := cmd.Output(); err != nil || !strings.Contains(string(out), `"messages"`) {
		t.Fatalf("prompt wrapper: %s, %v", out, err)
	}

	if err := Admit(target, "resources", []string{"doc://guide"}, Config{MCP: fake}); err != nil {
		t.Fatal(err)
	}
	read := filepath.Join(target, "bin", "read")
	if out, err := exec.Command(read, "doc://guide").Output(); err != nil || !strings.Contains(string(out), `"contents"`) {
		t.Fatalf("resource reader: %s, %v", out, err)
	}
	if err := exec.Command(read, "doc://secret").Run(); err == nil {
		t.Fatal("resource reader accepted an unadmitted URI")
	}

	if err := Admit(target, "templates", []string{"doc://guide/{chapter}"}, Config{MCP: fake}); err != nil {
		t.Fatal(err)
	}
	templateRead := filepath.Join(target, "bin", "read-template")
	if out, err := exec.Command(templateRead, "doc://guide/{chapter}", "doc://guide/intro").Output(); err != nil || !strings.Contains(string(out), `"contents"`) {
		t.Fatalf("template reader: %s, %v", out, err)
	}
}

func TestMakeFailureLeavesNoDestination(t *testing.T) {
	root := t.TempDir()
	fake := filepath.Join(root, "mcp")
	writeFakeMCP(t, fake, true)
	target := filepath.Join(root, "broken.mcp")
	endpoint := admit.Endpoint{Type: "stdio", Command: "/bin/echo", Path: os.Getenv("PATH")}
	if err := Make(context.Background(), target, endpoint, Config{MCP: fake}); err == nil {
		t.Fatal("Make succeeded")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("failed generation left destination behind: %v", err)
	}
}

func writeFakeMCP(t *testing.T, path string, failList bool) {
	t.Helper()
	failure := ""
	if failList {
		failure = "exit 2"
	}
	body := `#!/bin/sh
case "$1" in
  discover)
    printf '%s\n' '{"resultType":"complete","_meta":{"io.modelcontextprotocol/serverInfo":{"name":"fake","version":"1"}},"supportedVersions":["2026-07-28"],"capabilities":{"tools":{},"prompts":{},"resources":{}}}'
    ;;
  request)
    ` + failure + `
    case "$2" in
      tools/list) printf '%s\n' '{"resultType":"complete","tools":[{"name":"echo","description":"echo exact JSON","inputSchema":{"type":"object"}},{"name":"remote-schema","description":"unsafe remote schema","inputSchema":{"$ref":"https://example.invalid/schema.json"}}]}' ;;
      prompts/list) printf '%s\n' '{"resultType":"complete","prompts":[{"name":"review","description":"review a change","arguments":[{"name":"tone"}]}]}' ;;
      resources/list) printf '%s\n' '{"resultType":"complete","resources":[{"uri":"doc://guide","name":"guide","description":"the guide"}]}' ;;
      resources/templates/list) printf '%s\n' '{"resultType":"complete","resourceTemplates":[{"uriTemplate":"doc://guide/{chapter}","name":"guide chapter","description":"one chapter"}]}' ;;
      *) exit 2 ;;
    esac
    ;;
  tool)
    input=$(sed -n '1p')
    test -n "$input" || input='{}'
    printf '{"content":[],"structuredContent":%s}\n' "$input"
    ;;
  prompt)
    printf '%s\n' '{"messages":[{"role":"user","content":{"type":"text","text":"review"}}]}'
    ;;
  read)
    printf '%s\n' '{"contents":[{"uri":"doc://guide","text":"hello"}]}'
    ;;
  template-read)
    printf '%s\n' '{"contents":[{"uri":"doc://guide/intro","text":"hello"}]}'
    ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}
