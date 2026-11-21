package codex

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"testing"
)

func TestInstalledCodexQueueSchema(t *testing.T) {
	path := os.Getenv("CROSSTALK_COMPAT_QUEUE_DB")
	if path == "" {
		t.Skip("CROSSTALK_COMPAT_QUEUE_DB not set; the harness-compat workflow sets it")
	}
	u := url.URL{Scheme: "file", Path: path}
	db, err := sql.Open("sqlite", u.String()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := checkQueueSchema(context.Background(), conn); err != nil {
		t.Fatal(err)
	}
}
