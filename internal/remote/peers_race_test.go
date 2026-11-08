package remote

import (
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestConcurrentUpsertAndRemoveKeepExactSet(t *testing.T) {
	dir := newDir(t)
	fp := func(i int) string {
		return strings.Repeat(strconv.FormatInt(int64(i%16), 16), 64-len(strconv.Itoa(i))) + strconv.Itoa(i)
	}
	const n = 12
	seed := NewPeers(dir)
	for i := 0; i < n; i++ {
		if err := seed.Upsert(Peer{Alias: "old" + strconv.Itoa(i), Address: "192.0.2.1:7745", Fingerprint: fp(i)}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			if err := NewPeers(dir).Remove("old" + strconv.Itoa(i)); err != nil {
				t.Errorf("remove %d: %v", i, err)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			if err := NewPeers(dir).Upsert(Peer{Alias: "new" + strconv.Itoa(i), Address: "192.0.2.1:7745", Fingerprint: fp(100 + i)}); err != nil {
				t.Errorf("upsert %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	ps, err := NewPeers(dir).List()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, p := range ps {
		got[p.Alias] = true
	}
	if len(ps) != n || len(got) != n {
		t.Fatalf("%d peers, want %d: %v", len(ps), n, got)
	}
	for i := 0; i < n; i++ {
		if !got["new"+strconv.Itoa(i)] {
			t.Errorf("new%d missing", i)
		}
	}
}
