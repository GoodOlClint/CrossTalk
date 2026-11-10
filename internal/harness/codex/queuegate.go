package codex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"modernc.org/sqlite"

	"github.com/GoodOlClint/crosstalk/internal/ownerproof"
)

// QueueFile is Codex's queue store in its home directory. CrossTalk never
// creates it.
const QueueFile = "queue_1.sqlite"

var (
	ErrQueueGate      = errors.New("codex queue: gate failed")
	ErrQueueBusy      = errors.New("codex queue: database busy past the timeout")
	ErrQueueAmbiguous = errors.New("codex queue: commit outcome unknown")
)

// QueueTestedMinors are the Codex major.minor pairs whose queue the step 0
// probes measured; any other version fails the gate.
var QueueTestedMinors = []string{"0.167"}

const DefaultQueueBusyTimeout = 2 * time.Second

// queueSchemaSHA256 is the SHA-256 of sqlite_master in queue_1.sqlite at
// Codex 0.167.0, as queueSchemaDigest computes it: every table, index and
// trigger with its SQL text.
const queueSchemaSHA256 = "7def6f4e95ecc0edc0c0d884b1cb681062c9c1019098640f70c516b7549bcc11"

type queueMigration struct {
	version     int64
	description string
	checksum    string
}

// queueMigrations are the _sqlx_migrations rows at Codex 0.167.0; the
// checksums are sqlx's SHA-384 of each migration's SQL.
var queueMigrations = []queueMigration{
	{1, "queued items", "cea113c9a74fe4ed12cf41951b57c66ca375a14a722ac28d41bc06a096ed3e1d777ac0710c5377e8cd75f645c99ca03c"},
	{2, "queued thread revisions", "983a2b17b454feb00d7c221d7f556e900e7f9ade4aef395e854ed9093fa21237fd3fd93f99c66758b489c0f376f56570"},
}

// QueueGate opens Codex's queue for one transaction at a time and fails
// closed unless the file, its schema and the parent Codex version all match
// what the probes measured. No positive result is kept between calls.
type QueueGate struct {
	// Path is <codex home>/queue_1.sqlite.
	Path string
	// Parent is the Codex process whose version must be on the allowlist.
	Parent ownerproof.Proc
	// Version reads Parent's version; nil means ParentVersion.
	Version     func(context.Context, ownerproof.Proc) (string, error)
	BusyTimeout time.Duration

	beforeOpen   func()
	beforeBegin  func()
	afterBegin   func()
	beforeCommit func()
}

// Tx runs fn inside one transaction on the queue: BEGIN IMMEDIATE when
// write, else a deferred read. The file identity is checked against the path
// after BEGIN and again before COMMIT, and the schema and migrations are
// read inside the transaction, so a file replaced or migrated by another
// process between check and write fails closed. A COMMIT that errors is
// ErrQueueAmbiguous.
func (g *QueueGate) Tx(ctx context.Context, write bool, fn func(*sql.Conn) error) error {
	if err := g.checkVersion(ctx); err != nil {
		return err
	}
	before, err := os.Lstat(g.Path)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrQueueGate, err)
	}
	if !before.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrQueueGate, g.Path)
	}
	bt := g.BusyTimeout
	if bt <= 0 {
		bt = DefaultQueueBusyTimeout
	}
	u := url.URL{Scheme: "file", Path: g.Path}
	dsn := u.String() + fmt.Sprintf("?mode=rw&_pragma=busy_timeout(%d)", bt.Milliseconds())
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrQueueGate, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if g.beforeOpen != nil {
		g.beforeOpen()
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return queueErr(err)
	}
	defer conn.Close()
	if g.beforeBegin != nil {
		g.beforeBegin()
	}
	begin := "BEGIN"
	if write {
		begin = "BEGIN IMMEDIATE"
	}
	if _, err := conn.ExecContext(ctx, begin); err != nil {
		return queueErr(err)
	}
	committed := false
	defer func() {
		if !committed {
			conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
	}()
	if g.afterBegin != nil {
		g.afterBegin()
	}
	if err := g.sameFile(before); err != nil {
		return err
	}
	if err := checkQueueSchema(ctx, conn); err != nil {
		return err
	}
	if err := fn(conn); err != nil {
		return err
	}
	if g.beforeCommit != nil {
		g.beforeCommit()
	}
	if err := g.sameFile(before); err != nil {
		return err
	}
	committed = true
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("%w: %w", ErrQueueAmbiguous, err)
	}
	return nil
}

func (g *QueueGate) sameFile(before os.FileInfo) error {
	now, err := os.Lstat(g.Path)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrQueueGate, err)
	}
	if !os.SameFile(before, now) {
		return fmt.Errorf("%w: %s was replaced", ErrQueueGate, g.Path)
	}
	return nil
}

func (g *QueueGate) checkVersion(ctx context.Context) error {
	vf := g.Version
	if vf == nil {
		vf = ParentVersion
	}
	v, err := vf(ctx, g.Parent)
	if err != nil {
		return fmt.Errorf("%w: codex version: %w", ErrQueueGate, err)
	}
	if !queueVersionTested(v) {
		return fmt.Errorf("%w: codex %s is not in %v", ErrQueueGate, v, QueueTestedMinors)
	}
	return nil
}

func queueVersionTested(v string) bool {
	parts := strings.SplitN(v, ".", 3)
	return len(parts) == 3 && slices.Contains(QueueTestedMinors, parts[0]+"."+parts[1])
}

func queueErr(err error) error {
	var se *sqlite.Error
	if errors.As(err, &se) && se.Code()&0xff == 5 {
		return fmt.Errorf("%w: %w", ErrQueueBusy, err)
	}
	return fmt.Errorf("%w: %w", ErrQueueGate, err)
}

func queueSchemaDigest(ctx context.Context, conn *sql.Conn) (string, error) {
	rows, err := conn.QueryContext(ctx, "SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_master ORDER BY type, name")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	h := sha256.New()
	for rows.Next() {
		var typ, name, tbl, text string
		if err := rows.Scan(&typ, &name, &tbl, &text); err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\n", typ, name, tbl, text)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func checkQueueSchema(ctx context.Context, conn *sql.Conn) error {
	d, err := queueSchemaDigest(ctx, conn)
	if err != nil {
		return queueErr(err)
	}
	if d != queueSchemaSHA256 {
		return fmt.Errorf("%w: schema digest %s", ErrQueueGate, d)
	}
	rows, err := conn.QueryContext(ctx, "SELECT version, description, success, checksum FROM _sqlx_migrations ORDER BY version")
	if err != nil {
		return queueErr(err)
	}
	defer rows.Close()
	var got []queueMigration
	for rows.Next() {
		var m queueMigration
		var ok bool
		var sum []byte
		if err := rows.Scan(&m.version, &m.description, &ok, &sum); err != nil {
			return queueErr(err)
		}
		if !ok {
			return fmt.Errorf("%w: migration %d did not succeed", ErrQueueGate, m.version)
		}
		m.checksum = hex.EncodeToString(sum)
		got = append(got, m)
	}
	if err := rows.Err(); err != nil {
		return queueErr(err)
	}
	if !slices.Equal(got, queueMigrations) {
		return fmt.Errorf("%w: migrations %v", ErrQueueGate, got)
	}
	return nil
}

var (
	ErrExecutableReplaced = errors.New("codex: parent's executable is not the file at its path")
	versionRe             = regexp.MustCompile(`^codex-cli (\d+\.\d+\.\d+)\s*$`)
	versionCache          sync.Map
)

// VersionTimeout bounds one `codex --version` run within the caller's ctx.
var VersionTimeout = 5 * time.Second

// versionWaitDelay bounds the wait for a killed `codex --version` whose
// output pipe a child still holds open.
const versionWaitDelay = time.Second

// ParentVersion is p's Codex version, read once per (pid, start time) by
// running its executable with --version, and only while p's mapped
// executable is the file at that path; a failure is not cached.
func ParentVersion(ctx context.Context, p ownerproof.Proc) (string, error) {
	k := ownerKey(p)
	if v, ok := versionCache.Load(k); ok {
		return v.(string), nil
	}
	v, err := execVersion(ctx, p)
	if err != nil {
		return "", err
	}
	versionCache.Store(k, v)
	return v, nil
}

func execVersion(ctx context.Context, p ownerproof.Proc) (string, error) {
	exe, _, err := ownerproof.Args(p.PID)
	if err != nil {
		return "", err
	}
	before, err := os.Stat(exe)
	if err != nil {
		return "", err
	}
	same, err := ownerproof.ExecutableIs(p.PID, exe)
	if err != nil {
		return "", err
	}
	if !same {
		return "", ErrExecutableReplaced
	}
	ctx, cancel := context.WithTimeout(ctx, VersionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "--version")
	cmd.WaitDelay = versionWaitDelay
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("codex --version: %w", err)
	}
	after, err := os.Stat(exe)
	if err != nil {
		return "", err
	}
	if !os.SameFile(before, after) {
		return "", ErrExecutableReplaced
	}
	if now, err := ownerproof.Info(p.PID); err != nil || !now.Same(p) {
		return "", ErrParentChanged
	}
	m := versionRe.FindSubmatch(bytes.TrimSpace(out))
	if m == nil {
		return "", fmt.Errorf("codex --version: unparseable %q", out)
	}
	return string(m[1]), nil
}
