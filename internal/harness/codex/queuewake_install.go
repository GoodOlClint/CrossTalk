package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

const hookArgs = "hook codex session-start"

func hooksPath(home string) string { return filepath.Join(codexHome(home), "hooks.json") }

func stateDirOf(env harness.InstallEnv) string {
	if env.StateDir != "" {
		return env.StateDir
	}
	return filepath.Join(env.Home, ".local", "state", "crosstalk")
}

func hookCommand(bin string) string { return "'" + bin + "' " + hookArgs }

func isOurHook(h any) bool {
	m, ok := h.(map[string]any)
	if !ok {
		return false
	}
	c, _ := m["command"].(string)
	exe, found := strings.CutSuffix(c, " "+hookArgs)
	return found && filepath.Base(strings.Trim(exe, `'"`)) == "crosstalk"
}

func readHooks(path string) (map[string]any, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	doc := map[string]any{}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, true, fmt.Errorf("%s is not a JSON object: %w", path, err)
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

func legacyPath(env harness.InstallEnv) string {
	return filepath.Join(stateDirOf(env), LegacySettingFile)
}

func legacyStep(env harness.InstallEnv) (harness.Step, bool) {
	p := legacyPath(env)
	if _, err := os.Lstat(p); err != nil {
		return harness.Step{}, false
	}
	return harness.Step{
		Title:  "stale --no-daemon switch file",
		Change: "delete " + p + "; the switch is now crosstalk install --" + QueueWakeOption + " and this file has no effect",
		Apply: func(context.Context) error {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if env.Out != nil {
				fmt.Fprintf(env.Out, "    removed %s: the switch is now crosstalk install --%s\n", p, QueueWakeOption)
			}
			return nil
		},
	}, true
}

func queueWakeStep(env harness.InstallEnv) harness.Step {
	hp, sp := hooksPath(env.Home), filepath.Join(stateDirOf(env), SettingFile)
	return harness.Step{
		Title: "Codex --no-daemon queue wake (experimental, off unless you ask)",
		Change: fmt.Sprintf("create %s and add a SessionStart hook (%s) to %s (backup next to it).\n"+
			"A message to an idle codex --no-daemon session then queues a wake turn in Codex's local queue, and each wake is a paid model turn. macOS only. Codex may ask you to trust the new hook.", sp, hookCommand(env.Bin), hp),
		Apply: func(context.Context) error {
			if err := os.MkdirAll(filepath.Dir(sp), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(sp, nil, 0o600); err != nil {
				return err
			}
			return addHook(hp, env.Bin)
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
			return removeHook(hp)
		},
	}
}

func (h *Harness) k10(ctx context.Context, env harness.InstallEnv) harness.CheckResult {
	r := harness.CheckResult{ID: "K10", OK: true, WarnOnly: true}
	var stale string
	if _, err := os.Lstat(legacyPath(env)); err == nil {
		stale = fmt.Sprintf("; stale %s found, it has no effect (crosstalk install removes it)", legacyPath(env))
	}
	if !ClaimsEnabled(stateDirOf(env)) {
		r.Detail = "experimental --no-daemon queue wake is off (crosstalk install --" + QueueWakeOption + " turns it on)" + stale
		r.OK = stale == ""
		return r
	}
	var bad []string
	if runtime.GOOS != "darwin" {
		bad = append(bad, "the route is macOS only")
	}
	if !hasHook(hooksPath(env.Home)) {
		bad = append(bad, "the SessionStart hook is missing from "+hooksPath(env.Home))
	}
	if out, err := env.Run(ctx, "codex", "--version"); err != nil {
		bad = append(bad, "codex --version failed: "+err.Error())
	} else if m := versionRe.FindStringSubmatch(strings.TrimSpace(string(out))); m == nil || !queueVersionTested(m[1]) {
		bad = append(bad, fmt.Sprintf("codex %q is outside the tested versions %v, so the gate keeps the route off", strings.TrimSpace(string(out)), QueueTestedMinors))
	}
	if len(bad) > 0 {
		r.OK, r.Detail = false, "experimental --no-daemon queue wake is on but "+strings.Join(bad, "; ")+stale
		return r
	}
	r.OK, r.Detail = stale == "", "experimental --no-daemon queue wake is on: hook installed, codex version passes the gate"+stale
	return r
}
