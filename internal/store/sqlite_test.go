package store_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/aria/x-tester/internal/store"
)

func TestNodeStatePersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(-2 * time.Minute).Truncate(time.Second)
	if err := db.Save(store.NodeState{
		ID: "n1", Address: "1.2.3.4:443", Name: "a", Status: store.StatusActive,
		Country: "US", Latency: 50 * time.Millisecond, LastCheck: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Save(store.NodeState{
		ID: "n2", Address: "5.6.7.8:443", Name: "b", Status: store.StatusFailed,
		LastCheck: now, Reason: "timeout",
	}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	db2, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	s1, ok := db2.Get("n1")
	if !ok || s1.Status != store.StatusActive || s1.Country != "US" {
		t.Fatalf("active: %+v", s1)
	}
	if !s1.LastCheck.Equal(now) {
		t.Fatalf("last_check want %v got %v", now, s1.LastCheck)
	}
	s2, ok := db2.Get("n2")
	if !ok || s2.Status != store.StatusFailed {
		t.Fatalf("failed: %+v", s2)
	}
	if db2.CountByStatus(store.StatusActive) != 1 {
		t.Fatal("count active")
	}
}

func TestIgnoredPersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MarkIgnored(store.IgnoredNode{
		ID: "abc", Address: "1.2.3.4:443", Name: "n", Country: "IR", Reason: "filtered", At: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	db2, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if !db2.IsIgnored("abc") {
		t.Fatal("expected ignored after reopen")
	}
}

func TestUnignoreClearsLastCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	past := time.Now().Add(-time.Hour)
	if err := db.MarkIgnored(store.IgnoredNode{
		ID: "de1", Address: "1.1.1.1:443", Name: "de", Country: "DE", Reason: "filtered", At: past,
	}); err != nil {
		t.Fatal(err)
	}
	n, err := db.UnignoreIfAllowed(func(code string) bool { return code == "DE" })
	if err != nil || n != 1 {
		t.Fatalf("unignore: n=%d err=%v", n, err)
	}
	st, ok := db.Get("de1")
	if !ok || st.Status != store.StatusPending {
		t.Fatalf("status: %+v", st)
	}
	if !st.LastCheck.IsZero() {
		t.Fatalf("last_check should be cleared, got %v", st.LastCheck)
	}
}

func TestIgnoreIfDisallowed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := db.Save(store.NodeState{
		ID: "us1", Address: "2.2.2.2:443", Name: "us", Status: store.StatusActive, Country: "US",
	}); err != nil {
		t.Fatal(err)
	}
	n, err := db.IgnoreIfDisallowed(func(code string) bool { return code == "DE" })
	if err != nil || n != 1 {
		t.Fatalf("ignore: n=%d err=%v", n, err)
	}
	if !db.IsIgnored("us1") {
		t.Fatal("expected US active to become ignored")
	}
}
