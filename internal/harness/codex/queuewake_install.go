package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/GoodOlClint/crosstalk/internal/harness"
)

// QueueWakeOption is the install flag (without dashes) for the experimental
// --no-daemon queue wake.
const QueueWakeOption = "codex-no-daemon-queue"

// DeliveryQueueWake is the list_agents delivery_mode of a --no-daemon thread
// that the queue wake can reach.
const DeliveryQueueWake = "queue-wake (experimental)"

// QueueWakeOS is the platform the install step and K10 judge the route
// against; tests set it.
var QueueWakeOS = runtime.GOOS

const hookArgs = "hook codex session-start"

func hooksPath(home string) string { return filepath.Join(codexHome(home), "hooks.json") }

func stateDirOf(env harness.InstallEnv) string {
	if env.StateDir != "" {
		return env.StateDir
	}
	return filepath.Join(env.Home, ".local", "state", "crosstalk")
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func hookCommand(bin string) string { return shQuote(bin) + " " + hookArgs }

// isOurHook matches only a command that is exactly one shell-quoted absolute
// path to a crosstalk binary followed by hookArgs, so a user's compound
// command that ends in the same invocation is left alone.
func isOurHook(h any) bool {
	m, ok := h.(map[string]any)
	if !ok {
		return false
	}
	c, _ := m["command"].(string)
	exe, found := strings.CutSuffix(c, " "+hookArgs)
	if !found || !strings.HasPrefix(exe, "'") || !strings.HasSuffix(exe, "'") || len(exe) < 2 {
		return false
	}
	p := strings.ReplaceAll(exe[1:len(exe)-1], `'\''`, "'")
	return shQuote(p) == exe && filepath.IsAbs(p) && filepath.Base(p) == "crosstalk"
}

func readHooks(path string) (map[string]any, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil || doc == nil || dec.Decode(&struct{}{}) != io.EOF {
		return nil, true, fmt.Errorf("%s is not a single JSON object; CrossTalk wrote nothing", path)
	}
	if h, ok := doc["hooks"]; ok {
		hm, isObj := h.(map[string]any)
		if !isObj {
			return nil, true, fmt.Errorf("%s: \"hooks\" is not an object; CrossTalk wrote nothing", path)
		}
		if g, ok := hm["SessionStart"]; ok {
			if _, isArr := g.([]any); !isArr {
				return nil, true, fmt.Errorf("%s: \"hooks.SessionStart\" is not an array; CrossTalk wrote nothing", path)
			}
		}
	}
	return doc, true, nil
}

// stripOurs removes this program's SessionStart handlers from doc and drops
// the groups, the key and the "hooks" object that end up empty.
func stripOurs(doc map[string]any) {
	hooks, _ := doc["hooks"].(map[string]any)
	groups, _ := hooks["SessionStart"].([]any)
	var kept []any
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			kept = append(kept, g)
			continue
		}
		hs, _ := gm["hooks"].([]any)
		var rest []any
		for _, h := range hs {
			if !isOurHook(h) {
				rest = append(rest, h)
			}
		}
		if len(rest) == len(hs) {
			kept = append(kept, g)
			continue
		}
		if len(rest) > 0 {
			gm["hooks"] = rest
			kept = append(kept, gm)
		}
	}
	switch {
	case len(kept) > 0:
		hooks["SessionStart"] = kept
	case hooks != nil:
		delete(hooks, "SessionStart")
		if len(hooks) == 0 {
			delete(doc, "hooks")
		}
	}
}

func writeHooks(path string, doc map[string]any, existed bool) error {
	if len(doc) == 0 {
		if existed {
			return os.Remove(path)
		}
		return nil
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return harness.ReplaceFile(path, append(b, '\n'))
}

func addHook(path, bin string) error {
	doc, existed, err := readHooks(path)
	if err != nil {
		return err
	}
	stripOurs(doc)
	hooks, _ := doc["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		doc["hooks"] = hooks
	}
	groups, _ := hooks["SessionStart"].([]any)
	hooks["SessionStart"] = append(groups, map[string]any{"hooks": []any{map[string]any{"type": "command", "command": hookCommand(bin), "timeout": 5}}})
	return writeHooks(path, doc, existed)
}

func planRemoveHook(path string) (func() error, error) {
	doc, existed, err := readHooks(path)
	if err != nil || !existed {
		return nil, err
	}
	before, _ := json.Marshal(doc)
	stripOurs(doc)
	if after, _ := json.Marshal(doc); string(before) == string(after) {
		return nil, nil
	}
	return func() error { return writeHooks(path, doc, existed) }, nil
}

// ourHookRefs lists the "<group>:<handler>" position of each of this program's
// SessionStart handlers, the suffix of its hooks.state trust key.
func ourHookRefs(path string) []string {
	doc, _, err := readHooks(path)
	if err != nil {
		return nil
	}
	hooks, _ := doc["hooks"].(map[string]any)
	groups, _ := hooks["SessionStart"].([]any)
	var refs []string
	for gi, g := range groups {
		gm, _ := g.(map[string]any)
		hs, _ := gm["hooks"].([]any)
		for hi, h := range hs {
			if isOurHook(h) {
				refs = append(refs, fmt.Sprintf("%d:%d", gi, hi))
			}
		}
	}
	return refs
}

func hasHook(path string) bool {
	doc, _, err := readHooks(path)
	if err != nil {
		return false
	}
	hooks, _ := doc["hooks"].(map[string]any)
	groups, _ := hooks["SessionStart"].([]any)
	for _, g := range groups {
		gm, _ := g.(map[string]any)
		hs, _ := gm["hooks"].([]any)
		for _, h := range hs {
			if isOurHook(h) {
				return true
			}
		}
	}
	return false
}

func queueWakeStep(env harness.InstallEnv) harness.Step {
	hp, sp := hooksPath(env.Home), filepath.Join(stateDirOf(env), SettingFile)
	return harness.Step{
		Title: "Codex --no-daemon queue wake (experimental, off unless you ask)",
		Change: fmt.Sprintf("create %s and add a SessionStart hook (%s) to %s (backup next to it).\n"+
			"A message to an idle codex --no-daemon session then queues a wake turn in Codex's local queue, and each wake is a paid model turn. macOS only. Codex may ask you to trust the new hook.", sp, hookCommand(env.Bin), hp),
		Apply: func(context.Context) error {
			if QueueWakeOS != "darwin" {
				return errors.New("the queue wake is macOS only (operator ruling R197)")
			}
			if err := addHook(hp, env.Bin); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(sp), 0o700); err != nil {
				return err
			}
			return os.WriteFile(sp, nil, 0o600)
		},
	}
}

func queueWakeRemoveStep(env harness.InstallEnv) harness.Step {
	hp, sp := hooksPath(env.Home), filepath.Join(stateDirOf(env), SettingFile)
	return harness.Step{
		Title:  "Codex --no-daemon queue wake",
		Change: "delete " + sp + " and remove CrossTalk's SessionStart hook from " + hp + " (backup next to it); other hooks stay. A wake row already written may still fire once.",
		Apply: func(context.Context) error {
			writeHook, err := planRemoveHook(hp)
			if err != nil {
				return err
			}
			writeTrust, err := planRemoveTrustRecords(env, hp, ourHookRefs(hp))
			if err != nil {
				return err
			}
			if writeTrust != nil {
				if err := writeTrust(); err != nil {
					return err
				}
			}
			if writeHook != nil {
				if err := writeHook(); err != nil {
					return err
				}
			}
			if err := os.Remove(sp); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return nil
		},
	}
}

func (h *Harness) k10(ctx context.Context, env harness.InstallEnv) harness.CheckResult {
	r := harness.CheckResult{ID: "K10", OK: true, WarnOnly: true}
	if !ClaimsEnabled(stateDirOf(env)) {
		r.Detail = "experimental --no-daemon queue wake is off (crosstalk install --" + QueueWakeOption + " turns it on)"
		return r
	}
	var bad []string
	if QueueWakeOS != "darwin" {
		bad = append(bad, "the route is macOS only")
	}
	if !hasHook(hooksPath(env.Home)) {
		bad = append(bad, "the SessionStart hook is missing from "+hooksPath(env.Home))
	}
	if out, err := env.Run(ctx, "codex", "--version"); err != nil {
		bad = append(bad, "codex --version failed: "+err.Error())
	} else if m := versionRe.FindStringSubmatch(strings.TrimSpace(string(out))); m == nil {
		bad = append(bad, fmt.Sprintf("codex --version output %q does not parse, so the gate keeps the route off", strings.TrimSpace(string(out))))
	} else if !queueVersionAtLeastFloor(m[1]) {
		bad = append(bad, fmt.Sprintf("codex %q is below the version floor %s, so the gate keeps the route off", strings.TrimSpace(string(out)), QueueMinVersion))
	}
	if len(bad) > 0 {
		r.OK, r.Detail = false, "experimental --no-daemon queue wake is on but "+strings.Join(bad, "; ")
		return r
	}
	r.Detail = "experimental --no-daemon queue wake is on: hook installed, codex version passes the gate"
	return r
}

// planRemoveTrustRecords returns a writer that deletes the hooks.state table Codex wrote when the user
// trusted each removed hook: from its header to the next table header or the end
// of the file. Blank and comment lines that end the record stay, since they
// precede the next table. Lines inside a multi-line string or array are never
// read as headers.
func planRemoveTrustRecords(env harness.InstallEnv, hooksFile string, refs []string) (func() error, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	path := configPath(env.Home)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	drop := map[string]bool{}
	for _, r := range refs {
		drop[`[hooks.state."`+hooksFile+`:session_start:`+r+`"]`] = true
	}
	var out, pending []string
	inStr, skip, changed, depth := "", false, false, 0
	for _, l := range strings.SplitAfter(string(b), "\n") {
		t := strings.TrimSpace(l)
		open := inStr != "" || depth > 0
		if !open && strings.HasPrefix(t, "[") {
			out = append(out, pending...)
			pending, skip = nil, drop[t]
			changed = changed || skip
			if !skip {
				out = append(out, l)
			}
			continue
		}
		var d int
		inStr, d = tomlStringState(l, inStr)
		depth += d
		switch {
		case !skip:
			out = append(out, l)
		case !open && (t == "" || strings.HasPrefix(t, "#")):
			pending = append(pending, l)
		default:
			pending = nil
		}
	}
	if skip && (inStr != "" || depth > 0) {
		return nil, fmt.Errorf("codex config %s: the trust record for the removed hook ends inside an unclosed array or string; fix or delete it by hand and retry", path)
	}
	out = append(out, pending...)
	if !changed {
		return nil, nil
	}
	return func() error { return harness.ReplaceFile(path, []byte(strings.Join(out, ""))) }, nil
}

// tomlStringState is the multi-line string state ("", `"""` or `”'`) after
// line, given the state before it, and the net change in [ ] nesting depth on
// the line. Backslashes escape in basic strings; a quote or bracket inside a
// single-line string, a multi-line string or after # opens or closes nothing.
func tomlStringState(line, in string) (string, int) {
	depth := 0
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch in {
		case `"""`:
			if c == '\\' {
				i++
			} else if n := quoteRun(line[i:], '"'); n >= 3 {
				in, i = "", i+n-1
			}
		case `'''`:
			if n := quoteRun(line[i:], '\''); n >= 3 {
				in, i = "", i+n-1
			}
		default:
			switch {
			case c == '#':
				return "", depth
			case c == '[':
				depth++
			case c == ']':
				depth--
			case strings.HasPrefix(line[i:], `"""`):
				in, i = `"""`, i+2
			case strings.HasPrefix(line[i:], `'''`):
				in, i = `'''`, i+2
			case c == '"':
				for i++; i < len(line) && line[i] != '"'; i++ {
					if line[i] == '\\' {
						i++
					}
				}
			case c == '\'':
				for i++; i < len(line) && line[i] != '\''; i++ {
				}
			}
		}
	}
	return in, depth
}

// quoteRun is the length, capped at 5, of the run of q at the start of s: a
// multi-line string ends at the last three quotes of a run of 3 to 5.
func quoteRun(s string, q byte) int {
	n := 0
	for n < len(s) && n < 5 && s[n] == q {
		n++
	}
	return n
}
