package box

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/patrickyoung/mcp/internal/admit"
)

var safeName = regexp.MustCompile(`\A[A-Za-z0-9][A-Za-z0-9._-]*\z`)

type Config struct {
	MCP     string
	Stderr  io.Writer
	Headers string
}

type catalogSpec struct {
	kind       string
	file       string
	method     string
	resultKey  string
	capability string
	identity   string
}

var catalogs = []catalogSpec{
	{"tools", "tools.jsonl", "tools/list", "tools", "tools", "name"},
	{"prompts", "prompts.jsonl", "prompts/list", "prompts", "prompts", "name"},
	{"resources", "resources.jsonl", "resources/list", "resources", "resources", "uri"},
	{"templates", "templates.jsonl", "resources/templates/list", "resourceTemplates", "resources", "uriTemplate"},
}

func Make(ctx context.Context, target string, endpoint admit.Endpoint, cfg Config) error {
	if target == "" {
		return fmt.Errorf("missing destination directory")
	}
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("destination already exists: %s", target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	parent := filepath.Dir(abs)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, "."+filepath.Base(abs)+".stage-")
	if err != nil {
		return err
	}
	keepStage := false
	defer func() {
		if !keepStage {
			_ = os.RemoveAll(stage)
		}
	}()

	mcpPath, err := resolveMCP(cfg.MCP)
	if err != nil {
		return err
	}
	discovery, err := invoke(ctx, mcpPath, endpoint, "discover", "", nil, cfg.Stderr, cfg.Headers)
	if err != nil {
		return fmt.Errorf("discovering server: %w", err)
	}
	if err := writeJSON(filepath.Join(stage, "endpoint.json"), endpoint, 0o644); err != nil {
		return err
	}
	if err := writePrettyRaw(filepath.Join(stage, "discover.json"), discovery, 0o644); err != nil {
		return err
	}
	for _, dir := range []string{"catalog", "admit", "tools", "actions", "prompts", "resources", "bin"} {
		if err := os.Mkdir(filepath.Join(stage, dir), 0o755); err != nil {
			return err
		}
	}

	capabilities, err := discoveryCapabilities(discovery)
	if err != nil {
		return err
	}
	for _, spec := range catalogs {
		itemsPath := filepath.Join(stage, "catalog", spec.file)
		pagesPath := filepath.Join(stage, "catalog", strings.TrimSuffix(spec.file, ".jsonl")+".pages.jsonl")
		if _, advertised := capabilities[spec.capability]; advertised {
			items, pages, err := fetchCatalog(ctx, mcpPath, endpoint, spec, cfg.Stderr, cfg.Headers)
			if err != nil {
				return err
			}
			if err := writeLines(itemsPath, items); err != nil {
				return err
			}
			if err := writeLines(pagesPath, pages); err != nil {
				return err
			}
		} else {
			if err := os.WriteFile(itemsPath, nil, 0o644); err != nil {
				return err
			}
			if err := os.WriteFile(pagesPath, nil, 0o644); err != nil {
				return err
			}
		}
		if err := os.WriteFile(filepath.Join(stage, "admit", spec.kind+".tsv"), nil, 0o644); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(stage, "admit", "actions.tsv"), nil, 0o644); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(stage, "runtime.json"), map[string]any{
		"mcp": mcpPath,
	}, 0o644); err != nil {
		return err
	}
	if err := os.Rename(stage, abs); err != nil {
		return err
	}
	keepStage = true
	return nil
}

func List(w io.Writer, dir, kind string) error {
	spec, err := specFor(kind)
	if err != nil {
		return err
	}
	endpoint, discovery, entries, err := loadCatalog(dir, spec)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		digest, err := admit.Digest(spec.kind, endpoint, discovery, entry.raw)
		if err != nil {
			return err
		}
		description := oneLine(entry.description)
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\n", entry.id, digest, description); err != nil {
			return err
		}
	}
	return nil
}

func Admit(dir, kind string, names []string, cfg Config) error {
	if len(names) == 0 {
		return fmt.Errorf("name at least one capability to admit")
	}
	if strings.TrimSuffix(kind, "s") == "action" {
		return admitActions(dir, names, cfg)
	}
	spec, err := specFor(kind)
	if err != nil {
		return err
	}
	endpoint, discovery, entries, err := loadCatalog(dir, spec)
	if err != nil {
		return err
	}
	byName := make(map[string]catalogEntry, len(entries))
	for _, entry := range entries {
		byName[entry.id] = entry
	}
	admissions, err := readAdmissions(filepath.Join(dir, "admit", spec.kind+".tsv"))
	if err != nil {
		return err
	}
	for _, name := range names {
		entry, ok := byName[name]
		if !ok {
			return fmt.Errorf("%s %q is not in the discovered catalogue", spec.kind, name)
		}
		digest, err := admit.Digest(spec.kind, endpoint, discovery, entry.raw)
		if err != nil {
			return err
		}
		if (spec.kind == "tools" || spec.kind == "prompts") && !safeName.MatchString(name) {
			return fmt.Errorf("%s name %q is not a safe program name", spec.kind, name)
		}
		if spec.kind == "tools" {
			if ref, ok := externalSchemaRef(entry.raw); ok {
				return fmt.Errorf("tool %q contains unadmitted external schema reference %q", name, ref)
			}
		}
		admissions[name] = digest
	}
	if err := writeAdmissionsAtomic(filepath.Join(dir, "admit", spec.kind+".tsv"), admissions); err != nil {
		return err
	}
	switch spec.kind {
	case "tools":
		return renderTools(dir, endpoint, byName, admissions, cfg)
	case "prompts":
		return renderPrompts(dir, endpoint, byName, admissions, cfg)
	case "resources":
		return renderRead(dir, endpoint, byName, admissions, cfg)
	case "templates":
		return renderTemplateRead(dir, endpoint, admissions, cfg)
	}
	return nil
}

func Revoke(dir, kind string, names []string, cfg Config) error {
	if strings.TrimSuffix(kind, "s") == "action" {
		return revokeActions(dir, names)
	}
	spec, err := specFor(kind)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "admit", spec.kind+".tsv")
	admissions, err := readAdmissions(path)
	if err != nil {
		return err
	}
	for _, name := range names {
		if (spec.kind == "tools" || spec.kind == "prompts") && safeName.MatchString(name) {
			if err := os.Remove(filepath.Join(dir, spec.kind, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		delete(admissions, name)
	}
	if spec.kind == "resources" {
		endpoint, _, entries, err := loadCatalog(dir, spec)
		if err != nil {
			return err
		}
		byName := make(map[string]catalogEntry, len(entries))
		for _, entry := range entries {
			byName[entry.id] = entry
		}
		// Remove read authority before changing the record of it.
		if err := renderRead(dir, endpoint, byName, admissions, cfg); err != nil {
			return err
		}
	}
	if spec.kind == "templates" {
		if err := renderTemplateRead(dir, admit.Endpoint{}, admissions, cfg); err != nil {
			return err
		}
	}
	return writeAdmissionsAtomic(path, admissions)
}

func Show(w io.Writer, dir string) error {
	endpointRaw, err := os.ReadFile(filepath.Join(dir, "endpoint.json"))
	if err != nil {
		return err
	}
	var endpoint admit.Endpoint
	if err := json.Unmarshal(endpointRaw, &endpoint); err != nil {
		return err
	}
	endpointArgs := endpointArgv(endpoint)
	fmt.Fprint(w, "endpoint")
	for _, arg := range endpointArgs {
		fmt.Fprintf(w, "\t%s", arg)
	}
	fmt.Fprintln(w)
	for _, spec := range catalogs {
		entries, err := readCatalog(filepath.Join(dir, "catalog", spec.file), spec.identity)
		if err != nil {
			return err
		}
		admitted, err := readAdmissions(filepath.Join(dir, "admit", spec.kind+".tsv"))
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "%s\t%d discovered\t%d admitted\n", spec.kind, len(entries), len(admitted))
	}
	actions, err := readOptionalAdmissions(filepath.Join(dir, "admit", "actions.tsv"))
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "actions\t%d admitted\n", len(actions))
	return nil
}

func admitActions(dir string, names []string, cfg Config) error {
	spec, _ := specFor("tools")
	endpoint, discovery, entries, err := loadCatalog(dir, spec)
	if err != nil {
		return err
	}
	byName := make(map[string]catalogEntry, len(entries))
	for _, entry := range entries {
		byName[entry.id] = entry
	}
	path := filepath.Join(dir, "admit", "actions.tsv")
	admissions, err := readOptionalAdmissions(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "actions"), 0o755); err != nil {
		return err
	}
	for _, name := range names {
		entry, ok := byName[name]
		if !ok {
			return fmt.Errorf("tool %q is not in the discovered catalogue", name)
		}
		if !safeName.MatchString(name) {
			return fmt.Errorf("tool name %q is not a safe action connector name", name)
		}
		if ref, ok := externalSchemaRef(entry.raw); ok {
			return fmt.Errorf("tool %q contains unadmitted external schema reference %q", name, ref)
		}
		digest, err := admit.Digest(spec.kind, endpoint, discovery, entry.raw)
		if err != nil {
			return err
		}
		admissions[name] = digest
	}
	if err := writeAdmissionsAtomic(path, admissions); err != nil {
		return err
	}
	return renderActions(dir, endpoint, byName, admissions, cfg)
}

func revokeActions(dir string, names []string) error {
	path := filepath.Join(dir, "admit", "actions.tsv")
	admissions, err := readOptionalAdmissions(path)
	if err != nil {
		return err
	}
	for _, name := range names {
		if safeName.MatchString(name) {
			if err := os.Remove(filepath.Join(dir, "actions", name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		delete(admissions, name)
	}
	return writeAdmissionsAtomic(path, admissions)
}

func Diff(ctx context.Context, oldDir, newDir string, stdout, stderr io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, "diff", "-ru", oldDir, newDir)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return 1, nil
	}
	return 2, err
}

func fetchCatalog(ctx context.Context, mcpPath string, endpoint admit.Endpoint, spec catalogSpec, stderr io.Writer, headers string) (items, pages [][]byte, err error) {
	cursor := ""
	for {
		params := []byte("{}")
		if cursor != "" {
			params, _ = json.Marshal(map[string]string{"cursor": cursor})
		}
		page, callErr := invoke(ctx, mcpPath, endpoint, "request", spec.method, params, stderr, headers)
		if callErr != nil {
			return nil, nil, fmt.Errorf("%s: %w", spec.method, callErr)
		}
		canonicalPage, err := admit.Canonical(page)
		if err != nil {
			return nil, nil, fmt.Errorf("%s result: %w", spec.method, err)
		}
		pages = append(pages, canonicalPage)
		var body map[string]json.RawMessage
		if err := json.Unmarshal(page, &body); err != nil {
			return nil, nil, err
		}
		var batch []json.RawMessage
		if err := json.Unmarshal(body[spec.resultKey], &batch); err != nil {
			return nil, nil, fmt.Errorf("%s result has no %s array", spec.method, spec.resultKey)
		}
		for _, raw := range batch {
			canonical, err := admit.Canonical(raw)
			if err != nil {
				return nil, nil, err
			}
			items = append(items, canonical)
		}
		if err := json.Unmarshal(body["nextCursor"], &cursor); err != nil || cursor == "" {
			return items, pages, nil
		}
	}
}

// invoke runs the public mcp filter with literal argv and JSON stdin.
func invoke(ctx context.Context, mcpPath string, endpoint admit.Endpoint, command, method string, input []byte, stderr io.Writer, headers string) ([]byte, error) {
	args := []string{command}
	var headerFile *os.File
	if headers != "" {
		var err error
		headerFile, err = os.Open(headers)
		if err != nil {
			return nil, fmt.Errorf("opening HTTP headers: %w", err)
		}
		defer headerFile.Close()
		args = append(args, "-header-fd", "3")
	}
	if command == "request" {
		if method == "" {
			return nil, fmt.Errorf("internal request invocation has no method")
		}
		args = append(args, method)
	}
	args = append(args, "--")
	args = append(args, endpointArgv(endpoint)...)
	cmd := exec.CommandContext(ctx, mcpPath, args...)
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stderr = stderr
	if headerFile != nil {
		cmd.ExtraFiles = []*os.File{headerFile}
	}
	if endpoint.Path != "" {
		cmd.Env = replaceEnv(os.Environ(), "PATH", endpoint.Path)
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return stdout.Bytes(), fmt.Errorf("mcp exited %d", exitErr.ExitCode())
		}
		return stdout.Bytes(), err
	}
	return bytes.TrimSpace(stdout.Bytes()), nil
}

type catalogEntry struct {
	id          string
	description string
	raw         json.RawMessage
}

func loadCatalog(dir string, spec catalogSpec) (admit.Endpoint, []byte, []catalogEntry, error) {
	var endpoint admit.Endpoint
	raw, err := os.ReadFile(filepath.Join(dir, "endpoint.json"))
	if err != nil {
		return endpoint, nil, nil, err
	}
	if err := json.Unmarshal(raw, &endpoint); err != nil {
		return endpoint, nil, nil, err
	}
	discovery, err := os.ReadFile(filepath.Join(dir, "discover.json"))
	if err != nil {
		return endpoint, nil, nil, err
	}
	entries, err := readCatalog(filepath.Join(dir, "catalog", spec.file), spec.identity)
	return endpoint, discovery, entries, err
}

func readCatalog(path, identity string) ([]catalogEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var entries []catalogEntry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 16<<20)
	for scanner.Scan() {
		raw := append(json.RawMessage(nil), scanner.Bytes()...)
		var head map[string]json.RawMessage
		if err := json.Unmarshal(raw, &head); err != nil {
			return nil, err
		}
		var id, description string
		if err := json.Unmarshal(head[identity], &id); err != nil || id == "" {
			return nil, fmt.Errorf("catalogue entry has no %s", identity)
		}
		_ = json.Unmarshal(head["description"], &description)
		entries = append(entries, catalogEntry{id: id, description: description, raw: raw})
	}
	return entries, scanner.Err()
}

func readAdmissions(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 2 || fields[0] == "" || fields[1] == "" {
			return nil, fmt.Errorf("invalid admission line in %s", path)
		}
		out[fields[0]] = fields[1]
	}
	return out, scanner.Err()
}

func readOptionalAdmissions(path string) (map[string]string, error) {
	admissions, err := readAdmissions(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	return admissions, err
}

func writeAdmissionsAtomic(path string, admissions map[string]string) error {
	names := make([]string, 0, len(admissions))
	for name := range admissions {
		names = append(names, name)
	}
	sort.Strings(names)
	var body strings.Builder
	for _, name := range names {
		fmt.Fprintf(&body, "%s\t%s\n", name, admissions[name])
	}
	return writeAtomic(path, []byte(body.String()), 0o644)
}

func renderTools(dir string, endpoint admit.Endpoint, entries map[string]catalogEntry, admissions map[string]string, cfg Config) error {
	mcpPath, err := runtimeMCP(dir, cfg)
	if err != nil {
		return err
	}
	for name, digest := range admissions {
		entry, ok := entries[name]
		if !ok {
			continue
		}
		if !safeName.MatchString(name) {
			return fmt.Errorf("tool name %q is not a safe program name", name)
		}
		body := renderTool(mcpPath, endpoint, name, digest, entry.description)
		if err := writeAtomic(filepath.Join(dir, "tools", name), []byte(body), 0o755); err != nil {
			return err
		}
	}
	return nil
}

func renderActions(dir string, endpoint admit.Endpoint, entries map[string]catalogEntry, admissions map[string]string, cfg Config) error {
	mcpPath, err := runtimeMCP(dir, cfg)
	if err != nil {
		return err
	}
	for name, digest := range admissions {
		entry, ok := entries[name]
		if !ok {
			continue
		}
		var tool struct {
			InputSchema json.RawMessage `json:"inputSchema"`
		}
		if err := json.Unmarshal(entry.raw, &tool); err != nil || len(tool.InputSchema) == 0 {
			return fmt.Errorf("tool %q has no inputSchema", name)
		}
		description := oneLine(entry.description)
		if description == "" {
			description = "Invoke MCP tool " + name
		}
		descriptor, err := json.Marshal(struct {
			Version     int             `json:"version"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		}{1, name, description, tool.InputSchema})
		if err != nil {
			return err
		}
		body := renderAction(mcpPath, endpoint, name, digest, descriptor)
		if err := writeAtomic(filepath.Join(dir, "actions", name), []byte(body), 0o755); err != nil {
			return err
		}
	}
	return nil
}

func renderPrompts(dir string, endpoint admit.Endpoint, entries map[string]catalogEntry, admissions map[string]string, cfg Config) error {
	mcpPath, err := runtimeMCP(dir, cfg)
	if err != nil {
		return err
	}
	for name, digest := range admissions {
		entry, ok := entries[name]
		if !ok {
			continue
		}
		if !safeName.MatchString(name) {
			return fmt.Errorf("prompt name %q is not a safe program name", name)
		}
		var body strings.Builder
		fmt.Fprintln(&body, "#!/bin/sh")
		fmt.Fprintf(&body, "# %s\n", oneLine(entry.description))
		fmt.Fprintf(&body, "# admitted MCP descriptor %s\n", digest)
		writeRuntimePrefix(&body, endpoint)
		fmt.Fprintf(&body, "exec %s $mcp_headers prompt -expect %s %s --", shellQuote(mcpPath), shellQuote(digest), shellQuote(name))
		writeEndpointArgs(&body, endpoint)
		fmt.Fprintln(&body)
		if err := writeAtomic(filepath.Join(dir, "prompts", name), []byte(body.String()), 0o755); err != nil {
			return err
		}
	}
	return nil
}

func renderRead(dir string, endpoint admit.Endpoint, entries map[string]catalogEntry, admissions map[string]string, cfg Config) error {
	path := filepath.Join(dir, "bin", "read")
	if len(admissions) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	mcpPath, err := runtimeMCP(dir, cfg)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(admissions))
	for name := range admissions {
		if _, ok := entries[name]; ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var body strings.Builder
	fmt.Fprintln(&body, "#!/bin/sh")
	fmt.Fprintln(&body, "# read one explicitly admitted MCP resource URI")
	fmt.Fprintln(&body, "test \"$#\" -eq 1 || { echo 'usage: read URI' >&2; exit 2; }")
	fmt.Fprintln(&body, "case $1 in")
	for _, uri := range names {
		fmt.Fprintf(&body, "  %s) expect=%s ;;\n", shellQuote(uri), shellQuote(admissions[uri]))
	}
	fmt.Fprintln(&body, "  *) echo \"read: resource URI is not admitted: $1\" >&2; exit 2 ;;")
	fmt.Fprintln(&body, "esac")
	writeRuntimePrefix(&body, endpoint)
	fmt.Fprintf(&body, "exec %s $mcp_headers read -expect \"$expect\" \"$1\" --", shellQuote(mcpPath))
	writeEndpointArgs(&body, endpoint)
	fmt.Fprintln(&body)
	return writeAtomic(path, []byte(body.String()), 0o755)
}

func renderTemplateRead(dir string, endpoint admit.Endpoint, admissions map[string]string, cfg Config) error {
	path := filepath.Join(dir, "bin", "read-template")
	if len(admissions) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if endpoint.Type == "" {
		var err error
		endpoint, _, _, err = loadCatalog(dir, catalogs[0])
		if err != nil {
			return err
		}
	}
	mcpPath, err := runtimeMCP(dir, cfg)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(admissions))
	for name := range admissions {
		names = append(names, name)
	}
	sort.Strings(names)
	var body strings.Builder
	fmt.Fprintln(&body, "#!/bin/sh")
	fmt.Fprintln(&body, "# read a URI through one explicitly admitted MCP resource template")
	fmt.Fprintln(&body, "test \"$#\" -eq 2 || { echo 'usage: read-template TEMPLATE URI' >&2; exit 2; }")
	fmt.Fprintln(&body, "case $1 in")
	for _, tmpl := range names {
		fmt.Fprintf(&body, "  %s) expect=%s ;;\n", shellQuote(tmpl), shellQuote(admissions[tmpl]))
	}
	fmt.Fprintln(&body, "  *) echo \"read-template: resource template is not admitted: $1\" >&2; exit 2 ;;")
	fmt.Fprintln(&body, "esac")
	writeRuntimePrefix(&body, endpoint)
	fmt.Fprintf(&body, "exec %s $mcp_headers template-read -expect \"$expect\" \"$1\" \"$2\" --", shellQuote(mcpPath))
	writeEndpointArgs(&body, endpoint)
	fmt.Fprintln(&body)
	return writeAtomic(path, []byte(body.String()), 0o755)
}

func renderTool(mcpPath string, endpoint admit.Endpoint, name, digest, description string) string {
	var body strings.Builder
	fmt.Fprintln(&body, "#!/bin/sh")
	fmt.Fprintf(&body, "# %s\n", oneLine(description))
	fmt.Fprintf(&body, "# admitted MCP descriptor %s\n", digest)
	writeRuntimePrefix(&body, endpoint)
	fmt.Fprintf(&body, "exec %s $mcp_headers tool -expect %s %s --", shellQuote(mcpPath), shellQuote(digest), shellQuote(name))
	writeEndpointArgs(&body, endpoint)
	fmt.Fprintln(&body)
	return body.String()
}

func renderAction(mcpPath string, endpoint admit.Endpoint, name, digest string, descriptor []byte) string {
	var body strings.Builder
	fmt.Fprintln(&body, "#!/bin/sh")
	fmt.Fprintf(&body, "# admitted effectful MCP tool %s descriptor %s\n", name, digest)
	fmt.Fprintln(&body, "case ${1-} in")
	fmt.Fprintln(&body, "  describe)")
	fmt.Fprintln(&body, "    test \"$#\" -eq 1 || { echo 'usage: connector describe' >&2; exit 2; }")
	fmt.Fprintf(&body, "    printf '%%s\\n' %s\n", shellQuote(string(descriptor)))
	fmt.Fprintln(&body, "    ;;")
	fmt.Fprintln(&body, "  run)")
	fmt.Fprintln(&body, "    test \"$#\" -eq 1 || { echo 'usage: connector run' >&2; exit 2; }")
	writeRuntimePrefix(&body, endpoint)
	fmt.Fprintf(&body, "    exec %s $mcp_headers tool -expect %s %s --", shellQuote(mcpPath), shellQuote(digest), shellQuote(name))
	writeEndpointArgs(&body, endpoint)
	fmt.Fprintln(&body)
	fmt.Fprintln(&body, "    ;;")
	fmt.Fprintln(&body, "  *) echo 'usage: connector describe|run' >&2; exit 2 ;;")
	fmt.Fprintln(&body, "esac")
	return body.String()
}

func writeRuntimePrefix(body *strings.Builder, endpoint admit.Endpoint) {
	if endpoint.Path != "" {
		fmt.Fprintf(body, "PATH=%s; export PATH\n", shellQuote(endpoint.Path))
	}
	if endpoint.Type == "http" {
		fmt.Fprintln(body, "mcp_headers=")
		fmt.Fprintln(body, "if test -n \"${MCP_HEADERS-}\"; then")
		fmt.Fprintln(body, "  exec 3<\"$MCP_HEADERS\" || exit 2")
		fmt.Fprintln(body, "  mcp_headers='-header-fd 3'")
		fmt.Fprintln(body, "fi")
	}
}

func endpointArgv(endpoint admit.Endpoint) []string {
	if endpoint.Type == "http" {
		return []string{endpoint.URL}
	}
	return append([]string{endpoint.Command}, endpoint.Args...)
}

func writeEndpointArgs(body *strings.Builder, endpoint admit.Endpoint) {
	for _, arg := range endpointArgv(endpoint) {
		fmt.Fprintf(body, " %s", shellQuote(arg))
	}
}

func runtimeMCP(dir string, cfg Config) (string, error) {
	if cfg.MCP != "" {
		return resolveMCP(cfg.MCP)
	}
	var runtime struct {
		MCP string `json:"mcp"`
	}
	raw, readErr := os.ReadFile(filepath.Join(dir, "runtime.json"))
	if readErr == nil && json.Unmarshal(raw, &runtime) == nil && runtime.MCP != "" {
		return runtime.MCP, nil
	}
	return resolveMCP("")
}

func discoveryCapabilities(raw []byte) (map[string]json.RawMessage, error) {
	var body struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("discovery result: %w", err)
	}
	if body.Capabilities == nil {
		body.Capabilities = make(map[string]json.RawMessage)
	}
	return body.Capabilities, nil
}

func specFor(kind string) (catalogSpec, error) {
	kind = strings.TrimSuffix(kind, "s")
	for _, spec := range catalogs {
		if strings.TrimSuffix(spec.kind, "s") == kind {
			return spec, nil
		}
	}
	return catalogSpec{}, fmt.Errorf("unknown capability kind %q", kind)
}

func resolveMCP(configured string) (string, error) {
	name := configured
	if name == "" {
		name = "mcp"
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("finding mcp: %w", err)
	}
	return filepath.Abs(path)
}

func writeJSON(path string, value any, mode os.FileMode) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), mode)
}

func writePrettyRaw(path string, raw []byte, mode os.FileMode) error {
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		return err
	}
	out.WriteByte('\n')
	return os.WriteFile(path, out.Bytes(), mode)
}

func writeLines(path string, lines [][]byte) error {
	var body bytes.Buffer
	for _, line := range lines {
		body.Write(bytes.TrimSpace(line))
		body.WriteByte('\n')
	}
	return os.WriteFile(path, body.Bytes(), 0o644)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".write-")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	ok = true
	return nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func oneLine(value string) string {
	line := strings.Join(strings.Fields(value), " ")
	if len(line) > 120 {
		line = strings.TrimSpace(line[:117]) + "..."
	}
	return line
}

func externalSchemaRef(raw []byte) (string, bool) {
	var descriptor map[string]any
	if json.Unmarshal(raw, &descriptor) != nil {
		return "", false
	}
	for _, field := range []string{"inputSchema", "outputSchema"} {
		if ref, ok := findExternalRef(descriptor[field]); ok {
			return ref, true
		}
	}
	return "", false
}

func findExternalRef(value any) (string, bool) {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "$ref" || key == "$dynamicRef" {
				if ref, ok := child.(string); ok && !strings.HasPrefix(ref, "#") {
					return ref, true
				}
			}
			if ref, ok := findExternalRef(child); ok {
				return ref, true
			}
		}
	case []any:
		for _, child := range value {
			if ref, ok := findExternalRef(child); ok {
				return ref, true
			}
		}
	}
	return "", false
}

func replaceEnv(environ []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(environ)+1)
	for _, entry := range environ {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return append(out, prefix+value)
}
