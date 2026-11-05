package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
)

// installFake emulates the parts of the claude and codex CLIs install uses,
// keeping their state under HOME and CODEX_HOME. Every call is logged to
// HOME/fake.log.
func installFake(name string, args []string) int {
	home := os.Getenv("HOME")
	f, err := os.OpenFile(filepath.Join(home, "fake.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 99
	}
	fmt.Fprintln(f, name+" "+strings.Join(args, " "))
	f.Close()
	if os.Getenv("INSTALL_FAKE_FAIL") == name+" "+strings.Join(args, " ") {
		fmt.Fprintln(os.Stderr, "forced failure")
		return 1
	}
	if name == "claude" {
		return installFakeClaude(home, args)
	}
	return installFakeCodex(args)
}

func installFakeJSON(path string) map[string]any {
	m := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

func installFakeWriteJSON(path string, v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, b, 0o644)
}

func installFakeClaude(home string, args []string) int {
	plugins := filepath.Join(home, "fake-claude-plugins.json")
	cj := filepath.Join(home, ".claude.json")
	a := strings.Join(args, " ")
	switch {
	case a == "plugin list --json":
		b, err := os.ReadFile(plugins)
		if err != nil {
			b = []byte("[]")
		}
		os.Stdout.Write(b)
	case a == "plugin install crosstalk@crosstalk", a == "plugin update crosstalk@crosstalk":
		installFakeWriteJSON(plugins, []map[string]any{{"id": "crosstalk@crosstalk", "version": mcpserver.Version}})
	case a == "plugin uninstall crosstalk":
		os.Remove(plugins)
	case strings.HasPrefix(a, "plugin marketplace "):
	case strings.HasPrefix(a, "mcp add --scope user crosstalk "):
		rest := args[5:]
		env := map[string]any{}
		for len(rest) > 1 && rest[0] == "-e" {
			k, v, _ := strings.Cut(rest[1], "=")
			env[k] = v
			rest = rest[2:]
		}
		if len(rest) < 2 || rest[0] != "--" {
			return 2
		}
		m := installFakeJSON(cj)
		srv, _ := m["mcpServers"].(map[string]any)
		if srv == nil {
			srv = map[string]any{}
		}
		if srv["crosstalk"] != nil {
			fmt.Fprintln(os.Stderr, "MCP server crosstalk already exists in user config")
			return 1
		}
		srv["crosstalk"] = map[string]any{"type": "stdio", "command": rest[1], "args": rest[2:], "env": env}
		m["mcpServers"] = srv
		installFakeWriteJSON(cj, m)
	case a == "mcp remove --scope user crosstalk":
		m := installFakeJSON(cj)
		srv, _ := m["mcpServers"].(map[string]any)
		if srv["crosstalk"] == nil {
			return 1
		}
		delete(srv, "crosstalk")
		installFakeWriteJSON(cj, m)
	case a == "mcp get crosstalk":
		srv, _ := installFakeJSON(cj)["mcpServers"].(map[string]any)
		if srv["crosstalk"] == nil {
			return 1
		}
		fmt.Println("crosstalk:")
	default:
		fmt.Fprintln(os.Stderr, "fake claude: unhandled", a)
		return 2
	}
	return 0
}

var (
	installFakeHeader = regexp.MustCompile(`^\s*\[\[?\s*([^\]]+?)\s*\]\]?\s*$`)
	installFakeKV     = regexp.MustCompile(`^\s*([A-Za-z_]+)\s*=\s*(.+?)\s*$`)
)

// installFakeTable returns the key = value lines of the TOML table header.
func installFakeTable(config, header string) (map[string]string, bool) {
	kv := map[string]string{}
	in, found := false, false
	for _, l := range strings.Split(config, "\n") {
		if m := installFakeHeader.FindStringSubmatch(l); m != nil {
			in = m[1] == header
			found = found || in
			continue
		}
		if m := installFakeKV.FindStringSubmatch(l); in && m != nil {
			kv[m[1]] = m[2]
		}
	}
	return kv, found
}

func installFakeCodex(args []string) int {
	codexHome := os.Getenv("CODEX_HOME")
	cache := filepath.Join(codexHome, "plugins", "cache", "crosstalk", "crosstalk")
	cfgb, _ := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	cfg := string(cfgb)
	a := strings.Join(args, " ")
	switch {
	case strings.HasPrefix(a, "plugin marketplace "):
	case a == "plugin add crosstalk@crosstalk":
		installFakeWriteJSON(filepath.Join(cache, mcpserver.Version, ".codex-plugin", "plugin.json"), map[string]any{"name": "crosstalk", "version": mcpserver.Version})
	case a == "plugin remove crosstalk@crosstalk":
		os.RemoveAll(cache)
	case a == "mcp list":
		var headers []string
		for _, l := range strings.Split(cfg, "\n") {
			if m := installFakeHeader.FindStringSubmatch(l); m != nil {
				headers = append(headers, m[1])
			}
		}
		for _, h := range headers {
			if strings.HasPrefix(h, "mcp_servers.crosstalk.") && !slices.Contains(headers, "mcp_servers.crosstalk") {
				fmt.Fprintln(os.Stderr, "invalid transport in `mcp_servers.crosstalk`")
				return 1
			}
		}
	case a == "mcp get crosstalk --json":
		kv, ok := installFakeTable(cfg, "mcp_servers.crosstalk")
		if !ok {
			fmt.Fprintln(os.Stderr, "No MCP server named 'crosstalk' found.")
			return 1
		}
		var cmd string
		var argv []string
		json.Unmarshal([]byte(kv["command"]), &cmd)
		json.Unmarshal([]byte(kv["args"]), &argv)
		env := map[string]string{}
		if e, ok := installFakeTable(cfg, "mcp_servers.crosstalk.env"); ok {
			for k, v := range e {
				var s string
				json.Unmarshal([]byte(v), &s)
				env[k] = s
			}
		}
		json.NewEncoder(os.Stdout).Encode(map[string]any{"name": "crosstalk", "transport": map[string]any{"type": "stdio", "command": cmd, "args": argv, "env": env}})
	default:
		fmt.Fprintln(os.Stderr, "fake codex: unhandled", a)
		return 2
	}
	return 0
}
