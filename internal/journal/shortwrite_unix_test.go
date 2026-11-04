//go:build unix

package journal

import (
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
)

func TestShortWriteLeavesJournalUsable(t *testing.T) {
	signal.Ignore(syscall.SIGXFSZ)
	t.Cleanup(func() { signal.Reset(syscall.SIGXFSZ) })
	dir := t.TempDir()
	j, _ := open(t, dir, Options{})
	if err := j.Append(Record{Type: TypeDedupe, Sender: "s", MessageID: "m1", State: "x"}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(Path(dir, agent))
	if err != nil {
		t.Fatal(err)
	}
	var orig syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &orig); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Setrlimit(syscall.RLIMIT_FSIZE, &orig) })
	lim := orig
	lim.Cur = uint64(st.Size()) + 20
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &lim); err != nil {
		t.Skip(err)
	}
	if err := j.Append(Record{Type: TypeDedupe, Sender: "s", MessageID: "m2", State: strings.Repeat("y", 200)}); err == nil {
		t.Fatal("append past the file-size limit succeeded")
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &orig); err != nil {
		t.Fatal(err)
	}
	if err := j.Append(Record{Type: TypeDedupe, Sender: "s", MessageID: "m3", State: "x"}); err != nil {
		t.Fatalf("append after failed write: %v", err)
	}
	rs, err := j.Records()
	if err != nil || len(rs) != 2 || rs[0].MessageID != "m1" || rs[1].MessageID != "m3" {
		t.Fatalf("Records = %v, %v; want m1, m3", rs, err)
	}
	if _, ok := j.Dedupe("s", "m2"); ok {
		t.Fatal("failed append was indexed")
	}
	j.Close()
	j2, _, err := Open(dir, agent, bind, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	j2.Close()
}
