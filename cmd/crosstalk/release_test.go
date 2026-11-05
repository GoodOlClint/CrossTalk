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
