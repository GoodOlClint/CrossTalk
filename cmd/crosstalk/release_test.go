package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
)

func releaseRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func releaseScript(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(filepath.Join("scripts", "release-build.sh"), args...)
	cmd.Dir = releaseRepoRoot(t)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestReleaseBuildLinux(t *testing.T) {
	if _, err := exec.LookPath("shasum"); err != nil {
		t.Skip("shasum not on PATH")
	}
	dir := t.TempDir()
	if out, err := releaseScript(t, nil, "build-linux", dir); err != nil {
		t.Fatalf("build-linux: %v\n%s", err, out)
	}
	const tag = "v9.9.9"
	if out, err := releaseScript(t, nil, "package-linux", dir, tag); err != nil {
		t.Fatalf("package-linux: %v\n%s", err, out)
	}
	for arch, machine := range map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64} {
		name := "crosstalk-" + tag + "-linux-" + arch + ".tar.gz"
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", arch, err)
		}
		gz, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		tr := tar.NewReader(gz)
		var entries []string
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			entries = append(entries, h.Name)
			if h.Name != "crosstalk" {
				continue
			}
			if h.Mode&0o777 != 0o755 {
				t.Errorf("%s: mode %o, want 755", arch, h.Mode&0o777)
			}
			bin, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "crosstalk-"+arch)
			if err := os.WriteFile(path, bin, 0o755); err != nil {
				t.Fatal(err)
			}
			ef, err := elf.Open(path)
			if err != nil {
				t.Fatalf("%s: not ELF: %v", arch, err)
			}
			if ef.Machine != machine {
				t.Errorf("%s: machine %v, want %v", arch, ef.Machine, machine)
			}
			ef.Close()
		}
		f.Close()
		if len(entries) != 1 || entries[0] != "crosstalk" {
			t.Errorf("%s: entries %v, want [crosstalk]", arch, entries)
		}
		sum := exec.Command("shasum", "-a", "256", "-c", name+".sha256")
		sum.Dir = dir
		if out, err := sum.CombinedOutput(); err != nil || !strings.Contains(string(out), "OK") {
			t.Errorf("%s: checksum: %v\n%s", arch, err, out)
		}
	}
}

func TestReleaseWorkflowLinuxOutsideSigning(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(releaseRepoRoot(t), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	steps := strings.Split(string(raw), "\n      - ")[1:]
	signing := regexp.MustCompile(`\b(codesign|notarytool|security|lipo)\b`)
	var publish string
	linux := 0
	for _, s := range steps {
		if !strings.Contains(s, "dist-linux") {
			continue
		}
		linux++
		if m := signing.FindString(s); m != "" {
			t.Errorf("step names dist-linux and runs %q:\n%s", m, s)
		}
		if strings.Contains(s, "gh release") {
			publish = s
		}
	}
	if linux < 3 {
		t.Errorf("%d steps name dist-linux, want build, package and publish", linux)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		for _, suffix := range []string{".tar.gz", ".tar.gz.sha256"} {
			want := `dist-linux/crosstalk-$TAG-linux-` + arch + suffix + `"`
			if !strings.Contains(publish, want) {
				t.Errorf("publish files=(...) lacks %s", want)
			}
		}
	}
}

func TestCheckIdentity(t *testing.T) {
	const want = "Developer ID Application: Test (ABCDE12345)"
	line := func(n int, name string) string {
		return "  " + strconv.Itoa(n) + ") 0123456789ABCDEF0123456789ABCDEF01234567 \"" + name + "\"\n"
	}
	cases := []struct {
		name   string
		output string
		ok     bool
	}{
		{"none", "     0 valid identities found\n", false},
		{"one", line(1, want) + "     1 valid identities found\n", true},
		{"two", line(1, want) + line(2, want) + "     2 valid identities found\n", false},
		{"near miss", line(1, want+" copy") + "     1 valid identities found\n", false},
		{"one among others", line(1, "Other") + line(2, want) + "     2 valid identities found\n", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bin := t.TempDir()
			fake := "#!/bin/sh\ncat <<'EOF'\n" + c.output + "EOF\n"
			if err := os.WriteFile(filepath.Join(bin, "security"), []byte(fake), 0o755); err != nil {
				t.Fatal(err)
			}
			out, err := releaseScript(t, []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "APPLE_CODESIGN_IDENTITY=" + want}, "check-identity", "unused.keychain-db")
			if (err == nil) != c.ok {
				t.Errorf("err = %v, want ok=%v\n%s", err, c.ok, out)
			}
		})
	}
}

func TestIsFinal(t *testing.T) {
	ver := "v" + mcpserver.Version
	cases := []struct {
		tag  string
		want string
		ok   bool
	}{
		{ver, "true", true},
		{ver + "-rc.1", "false", true},
		{ver + "-rc.10", "false", true},
		{ver + "-rc.01", "", false},
		{ver + "-rc.", "", false},
		{ver + "-rc.x", "", false},
		{"v0.0.1", "", false},
	}
	for _, c := range cases {
		t.Run(c.tag, func(t *testing.T) {
			out, err := releaseScript(t, nil, "is-final", c.tag)
			if (err == nil) != c.ok {
				t.Fatalf("err = %v, want ok=%v\n%s", err, c.ok, out)
			}
			if c.ok && strings.TrimSpace(out) != c.want {
				t.Errorf("stdout %q, want %q", out, c.want)
			}
			if !c.ok && strings.Contains(out, "true") {
				t.Errorf("rejected tag printed a verdict: %q", out)
			}
		})
	}
}

func TestFormulaRender(t *testing.T) {
	ver := mcpserver.Version
	tag := "v" + ver
	assets := []string{
		"crosstalk-" + tag + "-macos-universal.zip",
		"crosstalk-" + tag + "-linux-amd64.tar.gz",
		"crosstalk-" + tag + "-linux-arm64.tar.gz",
	}
	write := func(dir string, skip string) map[string]string {
		sums := map[string]string{}
		for _, a := range assets {
			body := []byte("fake " + a)
			if err := os.WriteFile(filepath.Join(dir, a), body, 0o644); err != nil {
				t.Fatal(err)
			}
			h := sha256.Sum256(body)
			sums[a] = hex.EncodeToString(h[:])
			if a == skip {
				continue
			}
			if err := os.WriteFile(filepath.Join(dir, a+".sha256"), []byte(sums[a]+"  "+a+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return sums
	}

	t.Run("render", func(t *testing.T) {
		mac, linux := t.TempDir(), t.TempDir()
		sums := map[string]string{}
		for _, d := range []string{mac, linux} {
			for k, v := range write(d, "") {
				sums[k] = v
			}
		}
		out, err := releaseScript(t, nil, "formula", tag, mac, linux)
		if err != nil {
			t.Fatalf("formula: %v\n%s", err, out)
		}
		want := []string{
			`homepage "https://getcrosstalk.dev"`,
			`bin.install "crosstalk"`,
		}
		for _, a := range assets {
			want = append(want,
				`url "https://github.com/GoodOlClint/CrossTalk/releases/download/`+tag+`/`+a+`"`,
				`sha256 "`+sums[a]+`"`)
		}
		for _, w := range want {
			if !strings.Contains(out, w) {
				t.Errorf("formula lacks %s\n%s", w, out)
			}
		}
		if strings.Contains(out, "version \"") {
			t.Errorf("formula sets version explicitly; brew audit scans it from the URL\n%s", out)
		}
		if _, err := exec.LookPath("ruby"); err == nil {
			cmd := exec.Command("ruby", "-c")
			cmd.Stdin = strings.NewReader(out)
			if o, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("ruby -c: %v\n%s", err, o)
			}
		}
	})

	t.Run("missing sha256", func(t *testing.T) {
		mac, linux := t.TempDir(), t.TempDir()
		write(mac, "")
		write(linux, assets[2])
		cmd := exec.Command(filepath.Join("scripts", "release-build.sh"), "formula", tag, mac, linux)
		cmd.Dir = releaseRepoRoot(t)
		var stdout strings.Builder
		cmd.Stdout = &stdout
		if err := cmd.Run(); err == nil {
			t.Error("formula succeeded with a missing .sha256")
		}
		if stdout.Len() != 0 {
			t.Errorf("stdout not empty on failure: %q", stdout.String())
		}
	})
}

const fakeGH = `#!/bin/sh
echo "gh $*" >> "$FAKE_DIR/log"
case "$1 $2" in
"pr list")
	case "$*" in
	*"--state merged"*) cat "$FAKE_DIR/merged" 2>/dev/null ;;
	*"--state open"*) cat "$FAKE_DIR/open" 2>/dev/null ;;
	esac
	exit 0
	;;
"pr create") echo "https://example.test/pr/1" ;;
"pr merge")
	case "$*" in
	*"--auto"*)
		if [ -e "$FAKE_DIR/autofail" ]; then
			echo "GraphQL: Auto merge is not allowed for this repository (enablePullRequestAutoMerge)" >&2
			exit 1
		fi
		;;
	esac
	;;
"pr checks")
	cat "$FAKE_DIR/checks_out" 2>/dev/null
	exit "$(cat "$FAKE_DIR/checks_rc" 2>/dev/null || echo 8)"
	;;
"pr view")
	n=$(cat "$FAKE_DIR/n" 2>/dev/null || echo 0)
	n=$((n + 1))
	echo "$n" > "$FAKE_DIR/n"
	line=$(sed -n "${n}p" "$FAKE_DIR/polls")
	[ -n "$line" ] || line=$(tail -n 1 "$FAKE_DIR/polls")
	echo "$line"
	;;
esac
exit 0
`

const fakeGit = `#!/bin/sh
echo "git $*" >> "$FAKE_DIR/log"
case "$1" in
ls-remote) [ -e "$FAKE_DIR/remote" ] || exit 2 ;;
esac
exit 0
`

func TestLandPR(t *testing.T) {
	const url = "https://example.test/pr/1"
	cases := []struct {
		name    string
		files   map[string]string
		timeout string
		ok      bool
		out     []string
		has     []string
		lacks   []string
	}{
		{"a never merges", map[string]string{"polls": "OPEN BLOCKED\n"}, "3", false,
			[]string{url, "BLOCKED"}, []string{"git push", "pr create", "pr merge --auto --squash"}, nil},
		{"b merges on second poll", map[string]string{"polls": "OPEN BLOCKED\nMERGED CLEAN\n"}, "30", true,
			nil, []string{"pr create", "pr merge --auto --squash"}, nil},
		{"c closed", map[string]string{"polls": "CLOSED UNKNOWN\n"}, "30", false,
			[]string{url}, nil, nil},
		{"d open PR reused", map[string]string{"open": url + "\n", "polls": "OPEN BLOCKED\nMERGED CLEAN\n"}, "30", true,
			[]string{url}, []string{"pr merge --auto --squash"}, []string{"pr create", "git push"}},
		{"e already merged", map[string]string{"merged": url + "\n"}, "30", true,
			nil, nil, []string{"pr create", "git push", "pr merge"}},
		{"g auto refused, direct merge", map[string]string{"autofail": "", "polls": "OPEN BLOCKED\nOPEN CLEAN\n"}, "30", true,
			[]string{"Auto merge is not allowed"}, []string{"pr merge --auto --squash", "pr merge --squash"}, nil},
		{"h auto refused, checks fail", map[string]string{"autofail": "", "checks_rc": "1", "checks_out": "1\n", "polls": "OPEN BLOCKED\n"}, "30", false,
			[]string{"Allow auto-merge", "checks failed"}, []string{"pr checks"}, []string{"pr merge --squash"}},
		{"j auto refused, no checks registered yet", map[string]string{"autofail": "", "checks_rc": "1", "polls": "OPEN BLOCKED\nOPEN CLEAN\n"}, "30", true,
			nil, []string{"pr merge --squash"}, nil},
		{"i auto refused, never clean", map[string]string{"autofail": "", "polls": "OPEN BLOCKED\n"}, "3", false,
			[]string{"Allow auto-merge", "BLOCKED"}, nil, []string{"pr merge --squash"}},
		{"f remote branch no PR", map[string]string{"remote": "", "polls": "MERGED CLEAN\n"}, "30", true,
			nil, []string{"pr create"}, []string{"git push"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := t.TempDir()
			bin := t.TempDir()
			for name, body := range map[string]string{"gh": fakeGH, "git": fakeGit} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for name, body := range c.files {
				if err := os.WriteFile(filepath.Join(fake, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			env := []string{
				"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
				"FAKE_DIR=" + fake, "LAND_PR_POLL=1", "LAND_PR_TIMEOUT=" + c.timeout,
			}
			out, err := releaseScript(t, env, "land-pr", "Owner/Repo", "branch-x", "the title")
			if (err == nil) != c.ok {
				t.Fatalf("err = %v, want ok=%v\n%s", err, c.ok, out)
			}
			for _, w := range c.out {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q\n%s", w, out)
				}
			}
			logb, _ := os.ReadFile(filepath.Join(fake, "log"))
			log := string(logb)
			for _, w := range c.has {
				if !strings.Contains(log, w) {
					t.Errorf("log lacks %q\n%s", w, log)
				}
			}
			for _, w := range c.lacks {
				if strings.Contains(log, w) {
					t.Errorf("log has %q\n%s", w, log)
				}
			}
			if n := strings.Count(log, "pr create"); n > 1 {
				t.Errorf("pr create ran %d times", n)
			}
		})
	}
}

func TestBumpMarketplaces(t *testing.T) {
	refRE := regexp.MustCompile(`("ref": ")[^"]*(")`)
	setup := func(t *testing.T, ref string) (env []string, files []string) {
		t.Helper()
		dir := t.TempDir()
		for i, src := range []string{".claude-plugin/marketplace.json", ".agents/plugins/marketplace.json"} {
			raw, err := os.ReadFile(filepath.Join(releaseRepoRoot(t), src))
			if err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(dir, []string{"claude.json", "codex.json"}[i])
			if err := os.WriteFile(dst, refRE.ReplaceAll(raw, []byte("${1}"+ref+"${2}")), 0o644); err != nil {
				t.Fatal(err)
			}
			files = append(files, dst)
		}
		return []string{"CLAUDE_MARKETPLACE=" + files[0], "CODEX_MARKETPLACE=" + files[1]}, files
	}
	read := func(t *testing.T, files []string) [2]string {
		t.Helper()
		var got [2]string
		for i, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			got[i] = string(b)
		}
		return got
	}

	t.Run("bumps exactly the two refs", func(t *testing.T) {
		env, files := setup(t, "v9.9.0")
		before := read(t, files)
		if out, err := releaseScript(t, env, "bump-marketplaces", "v9.9.9"); err != nil {
			t.Fatalf("bump: %v\n%s", err, out)
		}
		after := read(t, files)
		for i := range files {
			bl, al := strings.Split(before[i], "\n"), strings.Split(after[i], "\n")
			if len(bl) != len(al) {
				t.Fatalf("%s: line count changed", files[i])
			}
			changed := 0
			for j := range bl {
				if bl[j] == al[j] {
					continue
				}
				changed++
				if !strings.Contains(al[j], `"ref": "v9.9.9"`) {
					t.Errorf("%s: changed line %q", files[i], al[j])
				}
			}
			if changed != 1 {
				t.Errorf("%s: %d lines changed, want 1", files[i], changed)
			}
		}
	})

	t.Run("same tag is unchanged", func(t *testing.T) {
		env, files := setup(t, "v9.9.9")
		before := read(t, files)
		out, err := releaseScript(t, env, "bump-marketplaces", "v9.9.9")
		if err != nil || !strings.Contains(out, "unchanged") {
			t.Fatalf("err = %v, out %q", err, out)
		}
		if read(t, files) != before {
			t.Error("files changed")
		}
	})

	refused := []struct{ name, have, tag string }{
		{"rc tag", "v9.9.0", "v9.9.9-rc.1"},
		{"older than main", "v9.9.10", "v9.9.9"},
		{"not a tag", "v9.9.0", "9.9.9"},
	}
	for _, c := range refused {
		t.Run("refuses "+c.name, func(t *testing.T) {
			env, files := setup(t, c.have)
			before := read(t, files)
			if out, err := releaseScript(t, env, "bump-marketplaces", c.tag); err == nil {
				t.Errorf("succeeded\n%s", out)
			}
			if read(t, files) != before {
				t.Error("files changed")
			}
		})
	}

	t.Run("write failure is reported", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		env, files := setup(t, "v9.9.0")
		dir := filepath.Dir(files[0])
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o755) })
		out, err := releaseScript(t, env, "bump-marketplaces", "v9.9.9")
		if err == nil {
			t.Errorf("exit 0 although the write failed\n%s", out)
		}
		if strings.Contains(out, "ref ->") {
			t.Errorf("success message after failed write\n%s", out)
		}
	})

	t.Run("version order not string order", func(t *testing.T) {
		env, files := setup(t, "v9.9.10")
		if out, err := releaseScript(t, env, "bump-marketplaces", "v9.10.0"); err != nil {
			t.Fatalf("bump: %v\n%s", err, out)
		}
		for _, c := range read(t, files) {
			if !strings.Contains(c, `"ref": "v9.10.0"`) {
				t.Errorf("ref not bumped:\n%s", c)
			}
		}
	})
}

func TestReleaseWorkflowPostReleaseJobs(t *testing.T) {
	src := repoFile(t, ".github/workflows/release.yml")
	jobs := workflowJobs(src)

	trigger, _, _ := strings.Cut(src, "\npermissions:")
	if strings.Contains(trigger, "branches") {
		t.Error("release.yml has a branch trigger; the marketplace merge would start a release")
	}

	release := jobs["release"]
	if !regexp.MustCompile(`(?m)^    outputs:\n      final: \$\{\{ steps\.final\.outputs\.final \}\}$`).MatchString(release) {
		t.Error("release job does not export final from the final step")
	}
	var finalStep string
	for _, s := range strings.Split(release, "\n      - ")[1:] {
		if strings.Contains(s, "id: final") {
			finalStep = s
		}
	}
	if !strings.Contains(finalStep, `scripts/release-build.sh is-final "$TAG"`) || !strings.Contains(finalStep, `"$GITHUB_OUTPUT"`) {
		t.Errorf("step with id final does not write is-final to GITHUB_OUTPUT:\n%s", finalStep)
	}

	cases := []struct {
		job, needs, repo, branch string
	}{
		{"marketplace", "release", "CrossTalk", "marketplace-$TAG"},
		{"homebrew", "[release, marketplace]", "homebrew-tap", "crosstalk-$TAG"},
	}
	if !strings.Contains(jobs["homebrew"], "newer than $TAG") {
		t.Error("homebrew job does not refuse a tag older than the formula on tap main")
	}
	forbidden := regexp.MustCompile(`APPLE_|AC_API|(?m)^    environment: release\s*$|always\(\)|!cancelled\(\)`)
	for _, c := range cases {
		body := jobs[c.job]
		if body == "" {
			t.Errorf("release.yml has no job %s", c.job)
			continue
		}
		for _, want := range []string{
			"\n    needs: " + c.needs + "\n",
			"\n    if: needs.release.outputs.final == 'true'\n",
			"\n    runs-on: ubuntu-latest\n",
			"\n    environment: homebrew-tap\n",
			"\n    permissions: {}\n",
			"\n    timeout-minutes: 50\n",
			"uses: actions/create-github-app-token@v3\n",
			"client-id: ${{ vars.RELEASE_APP_CLIENT_ID }}",
			"private-key: ${{ secrets.RELEASE_APP_PRIVATE_KEY }}",
			"owner: GoodOlClint\n",
			"repositories: " + c.repo + "\n",
			"scripts/release-build.sh land-pr GoodOlClint/" + c.repo + ` "` + c.branch + `"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("job %s lacks %q", c.job, want)
			}
		}
		if m := forbidden.FindString(body); m != "" {
			t.Errorf("job %s contains %q", c.job, strings.TrimSpace(m))
		}
		if n := strings.Count(body, "repositories:"); n != 1 {
			t.Errorf("job %s mints a token for %d repository lists, want 1", c.job, n)
		}
	}
}

func TestReleaseBuildStampsTagIntoBinary(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(bin, "args.log")
	fake := "#!/bin/sh\necho \"$@\" >> " + log + "\nwhile [ $# -gt 0 ]; do [ \"$1\" = -o ] && : > \"$2\"; shift; done\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + bin + ":" + os.Getenv("PATH")}
	const stamp = "-X github.com/GoodOlClint/crosstalk/internal/mcpserver.Version=0.1.4-rc.1"
	for _, c := range []struct {
		tag  string
		want bool
	}{{"v0.1.4-rc.1", true}, {"", false}} {
		os.Remove(log)
		e := append(env, "TAG="+c.tag)
		if out, err := releaseScript(t, e, "build-linux", t.TempDir()); err != nil {
			t.Fatalf("tag %q: %v\n%s", c.tag, err, out)
		}
		b, _ := os.ReadFile(log)
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		if len(lines) != 2 {
			t.Fatalf("tag %q: %d go builds, want 2", c.tag, len(lines))
		}
		for _, l := range lines {
			if strings.Contains(l, stamp) != c.want {
				t.Errorf("tag %q: go build %q, stamp want %v", c.tag, l, c.want)
			}
		}
	}
}

func ciScopeRepo(t *testing.T, files map[string]string) (dir, base, head string) {
	t.Helper()
	dir = t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	write := func(rel, body string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.go", "package main\n")
	git("add", ".")
	git("commit", "-q", "-m", "base")
	base = git("rev-parse", "HEAD")
	for rel, body := range files {
		if src, ok := strings.CutPrefix(body, "rename:"); ok {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(dir, src), filepath.Join(dir, rel)); err != nil {
				t.Fatal(err)
			}
			continue
		}
		write(rel, body)
	}
	git("add", ".")
	git("commit", "-q", "--allow-empty", "-m", "head")
	head = git("rev-parse", "HEAD")
	return dir, base, head
}

func TestCIScope(t *testing.T) {
	zero := strings.Repeat("0", 40)
	cases := []struct {
		name  string
		files map[string]string
		base  string
		want  string
	}{
		{"docs only", map[string]string{"README.md": "x", "docs/setup.md": "x", "docs/plans/a.txt": "x", "plugins/crosstalk/NOTES.md": "x"}, "", "docs"},
		{"marketplace only", map[string]string{".claude-plugin/marketplace.json": "{}", ".agents/plugins/marketplace.json": "{}"}, "", "docs"},
		{"mixed with go file", map[string]string{"README.md": "x", "internal/a.go": "package a\n"}, "", "code"},
		{"workflow change", map[string]string{".github/workflows/ci.yml": "x"}, "", "code"},
		{"script change", map[string]string{"scripts/release-build.sh": "x"}, "", "code"},
		{"plugin manifest", map[string]string{"plugins/crosstalk/plugin.json": "{}"}, "", "code"},
		{"empty before sha", map[string]string{"README.md": "x"}, "-", "code"},
		{"zero before sha", map[string]string{"README.md": "x"}, zero, "code"},
		{"go file renamed into docs", map[string]string{"docs/x.md": "rename:main.go"}, "", "code"},
		{"no changes", map[string]string{}, "", "code"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir, base, head := ciScopeRepo(t, c.files)
			switch c.base {
			case "-":
				base = ""
			case "":
			default:
				base = c.base
			}
			script, err := filepath.Abs(filepath.Join(releaseRepoRoot(t), "scripts", "release-build.sh"))
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(script, "ci-scope", base, head)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("ci-scope: %v\n%s", err, out)
			}
			if got := strings.TrimSpace(string(out)); got != c.want {
				t.Errorf("ci-scope = %q, want %q", got, c.want)
			}
		})
	}
}

func TestCIWorkflowScopesTests(t *testing.T) {
	test := workflowJobs(repoFile(t, ".github/workflows/ci.yml"))["test"]
	steps := strings.Split(test, "\n      - ")[1:]
	find := func(sub string) string {
		for _, s := range steps {
			if strings.Contains(s, sub) {
				return s
			}
		}
		return ""
	}
	scope := find("release-build.sh ci-scope")
	if scope == "" || !strings.Contains(scope, "id: scope") || !strings.Contains(scope, "github.event.pull_request.base.sha") || !strings.Contains(scope, "github.event.before") || !strings.Contains(scope, `"$GITHUB_OUTPUT"`) {
		t.Errorf("no scope step classifying PR base and push before sha:\n%s", scope)
	}
	if !strings.Contains(test, "fetch-depth: 0") {
		t.Error("test job does not fetch history for the diff")
	}
	fast := find("-run '")
	if fast == "" || !strings.Contains(fast, "steps.scope.outputs.scope == 'docs'") || strings.Contains(fast, "-race") {
		t.Errorf("no docs-scoped fast test step without -race:\n%s", fast)
	}
	for _, name := range []string{"TestSetupDocCoversGaps", "TestUpgradeFromBundledServerDocumented", "TestManifestsMatchBinary", "TestMarketplacesPinToReleaseTag", "TestVersionIs015AndManifestsMatch", "TestSiteConfig", "TestSiteLinks", "TestSiteNamesEveryTool", "TestSkillCoversStatesAndCodes", "TestEvalFixtureNoticesMatchConst", "TestPluginShipsNoServer"} {
		if !strings.Contains(fast, name) {
			t.Errorf("fast step lacks %s", name)
		}
	}
	for _, sub := range []string{"go build ./...", "go test -race -timeout 10m ./...", "go vet ./...", "gofmt -l ."} {
		s := find(sub)
		if s == "" || !strings.Contains(s, "steps.scope.outputs.scope != 'docs'") {
			t.Errorf("step running %s is not skipped for docs scope:\n%s", sub, s)
		}
	}
	if strings.Contains(repoFile(t, ".github/workflows/ci.yml"), "paths:") {
		t.Error("ci.yml path-filters a trigger; required checks would stay pending")
	}
}

func TestReleaseWorkflowChecksMainCI(t *testing.T) {
	release := workflowJobs(repoFile(t, ".github/workflows/release.yml"))["release"]
	if strings.Contains(release, "go test") {
		t.Error("release job still runs go test")
	}
	var step string
	for _, s := range strings.Split(release, "\n      - ")[1:] {
		if strings.Contains(s, "name: Check main CI") {
			step = s
		}
	}
	if step == "" {
		t.Fatal("release job has no Check main CI step")
	}
	if strings.Contains(step, "status=success") {
		t.Error("Check main CI filters runs by status=success, so an in-progress main run fails the release at once")
	}
	if !regexp.MustCompile(`(?m)^\s*queued \| in_progress \|[^)]*\| missing\)`).MatchString(step) {
		t.Error("Check main CI does not treat a not-yet-created run (missing) like a queued one")
	}
	if !strings.Contains(step, `no ci.yml push run on main for $GITHUB_SHA after`) {
		t.Error("Check main CI does not fail with a no-run-found message after the wait")
	}
	for _, want := range []string{"actions/workflows/ci.yml/runs", "head_sha=$GITHUB_SHA", "event=push", "branch=main", "in_progress", "queued", "sleep 30", "1200", ".conclusion", "/jobs", "test (ubuntu-latest)", "test (macos-latest)", "GH_TOKEN: ${{ github.token }}"} {
		if !strings.Contains(step, want) {
			t.Errorf("Check main CI step lacks %q:\n%s", want, step)
		}
	}
	if strings.Contains(step, "check-runs") {
		t.Error("Check main CI reads commit check runs, which any workflow or App can satisfy")
	}
	if !regexp.MustCompile(`(?m)^    permissions:\n      contents: write\n      actions: read$`).MatchString(release) {
		t.Error("release job permissions are not contents: write, actions: read")
	}
}
