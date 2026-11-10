package codex

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/ownerproof"
)

// newQueueFixture builds a queue_1.sqlite in dir from the read-only schema
// dump of a real 0.167.0 store, in WAL mode, with the 0.167.0 migration rows.
func newQueueFixture(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, QueueFile)
	dump, err := os.ReadFile("testdata/queue_1-0.167.0.schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, "PRAGMA journal_mode=WAL")
	for _, stmt := range splitSchema(string(dump)) {
		if strings.HasPrefix(stmt, "CREATE TABLE sqlite_sequence") {
			continue
		}
		mustExec(t, db, stmt)
	}
	for _, m := range queueMigrations {
		sum, _ := hex.DecodeString(m.checksum)
		mustExec(t, db, "INSERT INTO _sqlx_migrations(version, description, success, checksum, execution_time) VALUES (?, ?, 1, ?, 0)", m.version, m.description, sum)
	}
	return path
}

// splitSchema splits a .schema dump into statements; a trigger body's inner
// semicolons end at its END.
func splitSchema(s string) []string {
	var out []string
	var cur strings.Builder
	inTrigger := false
	for line := range strings.SplitSeq(s, "\n") {
		if strings.TrimSpace(line) == "" && cur.Len() == 0 {
			continue
		}
		if cur.Len() == 0 && strings.HasPrefix(line, "CREATE TRIGGER") {
			inTrigger = true
		}
		cur.WriteString(line + "\n")
		end := strings.HasSuffix(strings.TrimSpace(line), ";")
		if inTrigger {
			end = strings.TrimSpace(line) == "END;"
		}
		if end {
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
			inTrigger = false
		}
	}
	return out
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func rowCount(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM queued_items").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// execFixture runs one statement on its own connection and closes it, so the
// last close checkpoints the WAL into the file.
func execFixture(t *testing.T, path, q string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec(t, db, q)
}

func openFixture(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func testGate(path string) *QueueGate {
	return &QueueGate{Path: path, Parent: ownerproof.Proc{PID: 4242, Start: time.Unix(1, 0)}, Version: func(ownerproof.Proc) (string, error) { return "0.167.0", nil }, BusyTimeout: 200 * time.Millisecond}
}

var plainRows atomic.Int64

func insertPlain(conn *sql.Conn) error {
	n := plainRows.Add(1)
	_, err := conn.ExecContext(context.Background(), "INSERT INTO queued_items(id, thread_id, payload_json, queue_order, created_at_ms, updated_at_ms) VALUES (?, 't1', '{}', ?, 0, 0)", fmt.Sprint("row-", n), n)
	return err
}

func TestQueueGate_FixtureFromRealDump_ExpectPinnedDigest(t *testing.T) {
	path := newQueueFixture(t, t.TempDir())
	db := openFixture(t, path)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	d, err := queueSchemaDigest(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	if d != queueSchemaSHA256 {
		t.Fatalf("digest %s, pinned %s", d, queueSchemaSHA256)
	}
}

func TestQueueGate_Table_ExpectFailClosedWithNoRow(t *testing.T) {
	cases := []struct {
		name string
		// between runs after a first successful write; nil means no first write.
		between func(t *testing.T, path string)
		setup   func(t *testing.T, path string)
		gate    func(g *QueueGate)
		wantErr error
		rows    int
	}{
		{name: "matching", rows: 1},
		{name: "extra migration", setup: func(t *testing.T, p string) {
			execFixture(t, p, "INSERT INTO _sqlx_migrations(version, description, success, checksum, execution_time) VALUES (3, 'later', 1, x'00', 0)")
		}, wantErr: ErrQueueGate},
		{name: "changed checksum", setup: func(t *testing.T, p string) {
			execFixture(t, p, "UPDATE _sqlx_migrations SET checksum = x'00' WHERE version = 2")
		}, wantErr: ErrQueueGate},
		{name: "failed migration", setup: func(t *testing.T, p string) {
			execFixture(t, p, "UPDATE _sqlx_migrations SET success = 0 WHERE version = 2")
		}, wantErr: ErrQueueGate},
		{name: "missing trigger", setup: func(t *testing.T, p string) {
			execFixture(t, p, "DROP TRIGGER queued_items_revision_after_insert")
		}, wantErr: ErrQueueGate},
		{name: "changed trigger", setup: func(t *testing.T, p string) {
			execFixture(t, p, "DROP TRIGGER queued_items_revision_after_delete")
			execFixture(t, p, "CREATE TRIGGER queued_items_revision_after_delete AFTER DELETE ON queued_items BEGIN SELECT 1; END")
		}, wantErr: ErrQueueGate},
		{name: "extra column", setup: func(t *testing.T, p string) {
			execFixture(t, p, "ALTER TABLE queued_items ADD COLUMN extra TEXT")
		}, wantErr: ErrQueueGate},
		{name: "extra index", setup: func(t *testing.T, p string) {
			execFixture(t, p, "CREATE INDEX extra_idx ON queued_items(thread_id)")
		}, wantErr: ErrQueueGate},
		{name: "missing file", setup: func(t *testing.T, p string) {
			for _, s := range []string{"", "-wal", "-shm"} {
				os.Remove(p + s)
			}
		}, wantErr: ErrQueueGate, rows: -1},
		{name: "symlink", setup: func(t *testing.T, p string) {
			real := p + ".real"
			if err := os.Rename(p, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, p); err != nil {
				t.Fatal(err)
			}
		}, wantErr: ErrQueueGate},
		{name: "untested version", gate: func(g *QueueGate) {
			g.Version = func(ownerproof.Proc) (string, error) { return "0.168.0", nil }
		}, wantErr: ErrQueueGate},
		{name: "unreadable version", gate: func(g *QueueGate) {
			g.Version = func(ownerproof.Proc) (string, error) { return "", ErrExecutableReplaced }
		}, wantErr: ErrQueueGate},
		{name: "file replaced between writes", between: func(t *testing.T, p string) {
			other := newQueueFixture(t, t.TempDir())
			execFixture(t, other, "DROP TRIGGER queued_items_revision_after_update")
			replaceFile(t, other, p)
		}, wantErr: ErrQueueGate},
		{name: "migration applied between writes", between: func(t *testing.T, p string) {
			execFixture(t, p, "CREATE TABLE later (x)")
			execFixture(t, p, "INSERT INTO _sqlx_migrations(version, description, success, checksum, execution_time) VALUES (3, 'later', 1, x'01', 0)")
		}, wantErr: ErrQueueGate, rows: 1},
		{name: "file replaced inside the transaction", gate: func(g *QueueGate) {
			other := newQueueFixture(t, t.TempDir())
			g.afterBegin = func() { replaceFile(t, other, g.Path) }
		}, wantErr: ErrQueueGate},
		{name: "file replaced before commit", gate: func(g *QueueGate) {
			other := newQueueFixture(t, t.TempDir())
			g.beforeCommit = func() { replaceFile(t, other, g.Path) }
		}, wantErr: ErrQueueGate},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := newQueueFixture(t, dir)
			original := path + ".orig"
			if err := os.Link(path, original); err != nil {
				t.Fatal(err)
			}
			if c.setup != nil {
				c.setup(t, path)
			}
			g := testGate(path)
			if c.gate != nil {
				c.gate(g)
			}
			if c.between != nil {
				if err := g.Tx(context.Background(), true, insertPlain); err != nil {
					t.Fatalf("first write: %v", err)
				}
				c.between(t, path)
			}
			err := g.Tx(context.Background(), true, insertPlain)
			if c.wantErr == nil && err != nil || c.wantErr != nil && !errors.Is(err, c.wantErr) {
				t.Fatalf("err %v, want %v", err, c.wantErr)
			}
			if c.rows < 0 {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("gate created the missing file: %v", err)
				}
				return
			}
			if n := rowCount(t, path); n != c.rows {
				t.Fatalf("rows at path %d, want %d", n, c.rows)
			}
			if c.wantErr != nil && c.between == nil {
				if n := rowCount(t, original); n != 0 {
					t.Fatalf("rows in the original file %d, want 0", n)
				}
			}
		})
	}
}

// replaceFile renames a copy of src over dst, as a migration that rebuilt
// the file would: dst then names a new inode.
func replaceFile(t *testing.T, src, dst string) {
	t.Helper()
	tmp := dst + ".new"
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	out.Close()
	for _, s := range []string{"-wal", "-shm"} {
		os.Remove(dst + s)
	}
	if err := os.Rename(tmp, dst); err != nil {
		t.Fatal(err)
	}
}

func TestQueueGate_WriterHoldsLock_ExpectBusyAndNoRow(t *testing.T) {
	path := newQueueFixture(t, t.TempDir())
	db := openFixture(t, path)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = testGate(path).Tx(context.Background(), true, insertPlain)
	if !errors.Is(err, ErrQueueBusy) {
		t.Fatalf("err %v, want busy", err)
	}
	if d := time.Since(start); d < 150*time.Millisecond || d > 5*time.Second {
		t.Fatalf("busy after %v, want about the 200ms timeout", d)
	}
	conn.ExecContext(context.Background(), "ROLLBACK")
	if n := rowCount(t, path); n != 0 {
		t.Fatalf("rows %d", n)
	}
	if err := testGate(path).Tx(context.Background(), true, insertPlain); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

// The sqlite3 command line tool is C SQLite, as Codex links: its write lock
// must hold this driver off, and this driver's write must be visible to it.
func TestQueueGate_CSQLiteProcessHoldsLock_ExpectBusyThenWriteVisible(t *testing.T) {
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("no sqlite3 binary")
	}
	path := newQueueFixture(t, t.TempDir())
	cmd := exec.Command(bin, path)
	stdin, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close(); cmd.Process.Kill(); cmd.Wait() })
	r := bufio.NewReader(out)
	say := func(s string) string {
		io.WriteString(stdin, s+"\n.print done\n")
		var got strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				t.Fatalf("sqlite3: %v", err)
			}
			if line == "done\n" {
				return got.String()
			}
			got.WriteString(line)
		}
	}
	if got := say("PRAGMA journal_mode;"); got != "wal\n" {
		t.Fatalf("journal_mode %q", got)
	}
	say("BEGIN IMMEDIATE;")
	if err := testGate(path).Tx(context.Background(), true, insertPlain); !errors.Is(err, ErrQueueBusy) {
		t.Fatalf("err %v while C SQLite holds the write lock, want busy", err)
	}
	say("COMMIT;")
	if err := testGate(path).Tx(context.Background(), true, insertPlain); err != nil {
		t.Fatal(err)
	}
	if got := say("SELECT COUNT(*) FROM queued_items; SELECT revision > 0 FROM queued_thread_revisions WHERE thread_id = 't1';"); got != "1\n1\n" {
		t.Fatalf("C SQLite sees %q, want one row and a bumped revision", got)
	}
}

func TestQueueGate_ReadTx_ExpectNoWriteLock(t *testing.T) {
	path := newQueueFixture(t, t.TempDir())
	db := openFixture(t, path)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	if err := testGate(path).Tx(context.Background(), false, func(*sql.Conn) error { return nil }); err != nil {
		t.Fatalf("read while another connection writes: %v", err)
	}
}

func TestQueueGate_VersionCache_ExpectOneReadPerParentPidAndStart(t *testing.T) {
	path := newQueueFixture(t, t.TempDir())
	calls := 0
	versions := map[string]string{}
	vf := func(p ownerproof.Proc) (string, error) {
		k := ownerKey(p)
		if v, ok := versions[k]; ok {
			return v, nil
		}
		calls++
		versions[k] = "0.167.0"
		return "0.167.0", nil
	}
	g := testGate(path)
	g.Version = vf
	for range 3 {
		if err := g.Tx(context.Background(), false, func(*sql.Conn) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	g.Parent.Start = g.Parent.Start.Add(time.Second)
	if err := g.Tx(context.Background(), false, func(*sql.Conn) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("version reads %d, want 2 (one per pid and start time)", calls)
	}
}

func TestQueueVersionTested(t *testing.T) {
	for v, want := range map[string]bool{"0.167.0": true, "0.167.12": true, "0.166.0": false, "0.168.0": false, "1.167.0": false, "0.167": false, "": false} {
		if got := queueVersionTested(v); got != want {
			t.Errorf("%q: %v, want %v", v, got, want)
		}
	}
}

// startFakeCodex runs a copy of the test binary as a stand-in Codex parent
// that answers --version; the copy can then be replaced under it.
func startFakeCodex(t *testing.T) (ownerproof.Proc, string) {
	t.Helper()
	realKernel(t)
	exe := filepath.Join(t.TempDir(), "codex")
	copyFile(t, os.Args[0], exe)
	t.Setenv(realHelperMode, "codex")
	cmd := exec.Command(exe)
	stdin, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("fake codex: %q %v", line, err)
	}
	t.Cleanup(func() { stdin.Close(); cmd.Process.Kill(); cmd.Wait() })
	p, err := ownerproof.Info(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	return p, exe
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		t.Fatal(err)
	}
}

func TestParentVersion_RealProcess_ExpectVersionFromItsExecutable(t *testing.T) {
	p, _ := startFakeCodex(t)
	v, err := execVersion(p)
	if err != nil || v != "0.167.0" {
		t.Fatalf("version %q %v", v, err)
	}
}

func TestParentVersion_ExecutableReplacedWhileRunning_ExpectFailure(t *testing.T) {
	p, exe := startFakeCodex(t)
	copyFile(t, os.Args[0], exe)
	if v, err := execVersion(p); !errors.Is(err, ErrExecutableReplaced) {
		t.Fatalf("version %q err %v, want ErrExecutableReplaced", v, err)
	}
}
