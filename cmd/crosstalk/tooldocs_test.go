package main

import (
	"context"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GoodOlClint/crosstalk/internal/harness/claude"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

func registeredTools(t *testing.T) []string {
	t.Helper()
	srv := mcpserver.New(mcpserver.Options{Registry: registry.New(t.TempDir())})
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := srv.MCP().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	slices.Sort(names)
	return names
}

func TestToolListHasAttachDetachGrant(t *testing.T) {
	tools := registeredTools(t)
	for _, w := range []string{"attach", "detach", "grant"} {
		if !slices.Contains(tools, w) {
			t.Errorf("tool %s is not registered: %v", w, tools)
		}
		if ok, _ := path.Match(claude.AllowRule, "mcp__crosstalk__"+w); !ok {
			t.Errorf("claude allow rule %q does not cover %s", claude.AllowRule, w)
		}
	}
	src := repoFile(t, "internal/harness/codex/install.go")
	if !strings.Contains(src, `default_tools_approval_mode = \"approve\"`) {
		t.Error("codex config does not set approve mode for every tool")
	}
	if strings.Contains(src, "mcp_servers.crosstalk.tools") {
		t.Error("codex install.go configures tools one by one")
	}
}

var countPhrase = regexp.MustCompile(`(?i)\b(one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve|[0-9]+)\s+(mcp |model-facing )?tools\b|\badds\s+(one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve)\s*[.,;]`)

func TestToolDocsNameEveryRegisteredTool(t *testing.T) {
	tools := registeredTools(t)
	design := repoFile(t, "docs/design.md")
	i := strings.Index(design, "## 8. Tool surface")
	j := strings.Index(design, "## 9. Lifecycle")
	if i < 0 || j < i {
		t.Fatal("design.md has no section 8")
	}
	for name, text := range map[string]string{
		"docs/design.md section 8": design[i:j],
		"README.md":                repoFile(t, "README.md"),
		"AGENTS.md":                repoFile(t, "AGENTS.md"),
		"the skill":                repoFile(t, "plugins/crosstalk/skills/crosstalk/SKILL.md"),
		"docs/examples.md":         repoFile(t, "docs/examples.md"),
		"docs/how-it-works.md":     repoFile(t, "docs/how-it-works.md"),
	} {
		for _, tool := range tools {
			if !strings.Contains(text, "`"+tool+"`") && !strings.Contains(text, tool+"(") {
				t.Errorf("%s never names %s", name, tool)
			}
		}
	}
	for _, f := range []string{"README.md", "AGENTS.md", "docs/design.md", "docs/examples.md", "docs/how-it-works.md", "docs/faq.md", "docs/setup.md", "docs/why.md", "docs/index.md", "plugins/crosstalk/skills/crosstalk/SKILL.md"} {
		if m := countPhrase.FindString(repoFile(t, f)); m != "" {
			t.Errorf("%s counts tools (%q); name them instead", f, m)
		}
	}
}
