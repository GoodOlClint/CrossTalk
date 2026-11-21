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
	"slices"
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

func removeHook(path string) error {
	doc, existed, err := readHooks(path)
	if err != nil || !existed {
		return err
	}
	before, _ := json.Marshal(doc)
	stripOurs(doc)
	if after, _ := json.Marshal(doc); string(before) == string(after) {
		return nil
	}
	return writeHooks(path, doc, existed)
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
			if err := os.Remove(sp); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			refs := ourHookRefs(hp)
			if err := removeHook(hp); err != nil {
				return err
			}
			return removeTrustRecords(context.Background(), env, hp, refs)
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

// removeTrustRecords deletes the hooks.state table Codex wrote when the user
// trusted each removed hook; other hooks' records stay.
func removeTrustRecords(ctx context.Context, env harness.InstallEnv, hooksFile string, refs []string) error {
	if len(refs) == 0 {
		return nil
	}
	path := configPath(env.Home)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	drop := map[string]bool{}
	for _, r := range refs {
		drop["hooks.state."+hooksFile+":session_start:"+r] = true
	}
	lines := strings.Split(string(b), "\n")
	ts := tables(lines)
	changed := false
	for i := len(ts) - 1; i >= 0; i-- {
		if drop[ts[i].name] {
			lines = slices.Delete(lines, ts[i].start, ts[i].end)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return harness.ReplaceFile(path, []byte(strings.Join(lines, "\n")))
}
