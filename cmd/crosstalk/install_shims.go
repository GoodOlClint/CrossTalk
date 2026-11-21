package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/GoodOlClint/crosstalk/internal/harness"
)

// shimProc is a running `crosstalk mcp` process. Current reports that it
// runs the installed binary's file, not one an upgrade has since replaced.
type shimProc struct {
	PID, PPID  int
	ParentCmd  string
	ParentPPID int
	Current    bool
}

// shimLister lists every running `crosstalk mcp` process, judged against the
// file at bin. Tests replace it so nothing reads the host's process table.
var shimLister = listShims

type shimParentKind int

const (
	parentDaemon shimParentKind = iota
	parentAppServer
	parentClaude
	parentOther
)

// classifyShimParent names what started a shim from its parent's command:
// a Codex app-server whose own parent is init is the managed daemon (it
// outlives every session); any other app-server belongs to one session.
func classifyShimParent(cmd string, parentPPID int) shimParentKind {
	f := strings.Fields(cmd)
	switch {
	case slices.Contains(f, "app-server") && len(f) > 0 && strings.Contains(filepath.Base(f[0]), "codex"):
		if parentPPID == 1 {
			return parentDaemon
		}
		return parentAppServer
	case len(f) > 0 && strings.Contains(strings.ToLower(filepath.Base(f[0])), "claude"):
		return parentClaude
	}
	return parentOther
}

// parseShimTable reads `ps -axo pid=,ppid=,command=` output.
// ponytail: a binary path with spaces in it is not recognised.
func parseShimTable(out string) []shimProc {
	type row struct {
		ppid int
		cmd  string
	}
	rows := map[int]row{}
	var pids []int
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		rows[pid] = row{ppid, strings.Join(f[2:], " ")}
		pids = append(pids, pid)
	}
	var shims []shimProc
	for _, pid := range pids {
		r := rows[pid]
		f := strings.Fields(r.cmd)
		if len(f) < 2 || filepath.Base(f[0]) != "crosstalk" || f[1] != "mcp" {
			continue
		}
		par := rows[r.ppid]
		shims = append(shims, shimProc{PID: pid, PPID: r.ppid, ParentCmd: par.cmd, ParentPPID: par.ppid})
	}
	return shims
}

// parseLsofInode returns the inode of the first txt entry (the executable)
// in `lsof -Fi` output.
func parseLsofInode(out string) (uint64, bool) {
	txt := false
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "f"):
			txt = l == "ftxt"
		case txt && strings.HasPrefix(l, "i"):
			n, err := strconv.ParseUint(l[1:], 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

// procInode is the inode of the file pid executes: through /proc on Linux
// (it names the file even after a rename replaced it), through lsof on
// macOS, where ps shows only the path the process was started with.
func procInode(pid int) (uint64, bool) {
	if runtime.GOOS == "linux" {
		fi, err := os.Stat("/proc/" + strconv.Itoa(pid) + "/exe")
		if err != nil {
			return 0, false
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		return uint64(st.Ino), ok
	}
	out, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-d", "txt", "-Fi").Output()
	if err != nil && len(out) == 0 {
		return 0, false
	}
	return parseLsofInode(string(out))
}

func inodeOf(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}

// procRunsBin reports whether pid executes the file at bin. A process whose
// file cannot be read counts as current: no warning on a guess.
func procRunsBin(pid int, bin string) bool {
	fi, err := os.Stat(bin)
	if err != nil {
		return true
	}
	ino, ok := procInode(pid)
	return !ok || ino == inodeOf(fi)
}

func listShims(bin string) ([]shimProc, error) {
	if _, err := os.Stat(bin); err != nil {
		return nil, err
	}
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,command=").Output()
	if err != nil {
		return nil, err
	}
	shims := parseShimTable(string(out))
	for i := range shims {
		shims[i].Current = procRunsBin(shims[i].PID, bin)
	}
	return shims, nil
}

var shimGroupText = map[shimParentKind]struct{ who, fix string }{
	parentDaemon:    {"the managed Codex daemon", "run: codex app-server daemon restart (it interrupts running Codex turns)"},
	parentAppServer: {"standalone Codex app-servers", "end each of those Codex sessions and start it again; its app-server then starts a new shim"},
	parentClaude:    {"Claude Code sessions", "restart each session (in Claude, /mcp and reconnect also works)"},
	parentOther:     {"other parents", "restart the process that started each one"},
}

// installShimChecks is K11, warn-only: it lists running crosstalk mcp
// processes that still run a binary an upgrade replaced, by parent kind,
// with the restart for each. It signals no process. It says nothing when the
// process table cannot be read.
func installShimChecks(bin string) []harness.CheckResult {
	shims, err := shimLister(bin)
	if err != nil {
		return nil
	}
	groups := map[shimParentKind][]shimProc{}
	for _, s := range shims {
		if !s.Current {
			k := classifyShimParent(s.ParentCmd, s.ParentPPID)
			groups[k] = append(groups[k], s)
		}
	}
	if len(groups) == 0 {
		return []harness.CheckResult{{ID: "K11", OK: true, WarnOnly: true, Detail: fmt.Sprintf("%d running crosstalk mcp process(es), all on %s", len(shims), bin)}}
	}
	var rs []harness.CheckResult
	for k := parentDaemon; k <= parentOther; k++ {
		g := groups[k]
		if len(g) == 0 {
			continue
		}
		t := shimGroupText[k]
		var pids, parents []string
		for _, s := range g {
			pids = append(pids, strconv.Itoa(s.PID))
			if p := strconv.Itoa(s.PPID); !slices.Contains(parents, p) {
				parents = append(parents, p)
			}
		}
		rs = append(rs, harness.CheckResult{ID: "K11", WarnOnly: true, Detail: fmt.Sprintf(
			"%d crosstalk mcp process(es) under %s still run a binary an upgrade replaced (pids %s; parent pids %s); %s",
			len(g), t.who, capList(pids), capList(parents), t.fix)})
	}
	return rs
}

func capList(s []string) string {
	if len(s) > 10 {
		return strings.Join(s[:10], ", ") + fmt.Sprintf(" and %d more", len(s)-10)
	}
	return strings.Join(s, ", ")
}
