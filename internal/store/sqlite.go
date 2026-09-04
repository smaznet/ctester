package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	StatusPending = "pending"
	StatusActive  = "active"
	StatusFailed  = "failed"
	StatusIgnored = "ignored"
)

// DB persists probe status across restarts.
type DB struct {
	mu    sync.RWMutex
	sql   *sql.DB
	path  string
	cache map[string]NodeState // id -> state
}

type NodeState struct {
	ID        string
	Address   string
	Name      string
	SubURL    string
	Country   string
	ExitIP    string
	Status    string // active|failed|ignored|pending
	Latency   time.Duration
	FailCount int
	Reason    string
	LastCheck time.Time
}

// IgnoredNode kept for callers that only care about ignore rows.
type IgnoredNode struct {
	ID      string
	Address string
	Name    string
	Country string
	Reason  string
	At      time.Time
}

func Open(path string) (*DB, error) {
	if path == "" {
		path = "x-tester.db"
	}
	dir := filepath.Dir(filepath.Clean(path))
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1)
	if _, err := sqlDB.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	if _, err := sqlDB.Exec(`
CREATE TABLE IF NOT EXISTS node_states (
  id TEXT PRIMARY KEY,
  address TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL DEFAULT '',
  sub_url TEXT NOT NULL DEFAULT '',
  country TEXT NOT NULL DEFAULT '',
  exit_ip TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending',
  latency_ms INTEGER NOT NULL DEFAULT 0,
  fail_count INTEGER NOT NULL DEFAULT 0,
  reason TEXT NOT NULL DEFAULT '',
  last_check INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS ignored_nodes (
  id TEXT PRIMARY KEY,
  address TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL DEFAULT '',
  country TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL
);`); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	d := &DB{sql: sqlDB, path: path, cache: map[string]NodeState{}}
	if err := d.migrateIgnored(); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	if err := d.reload(); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return d, nil
}

func (d *DB) Path() string { return d.path }

func (d *DB) Close() error {
	if d == nil || d.sql == nil {
		return nil
	}
	return d.sql.Close()
}

func (d *DB) migrateIgnored() error {
	rows, err := d.sql.Query(`SELECT id, address, name, country, reason, updated_at FROM ignored_nodes`)
	if err != nil {
		return err
	}
	type row struct {
		id, addr, name, country, reason string
		ts                              int64
	}
	var list []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.addr, &r.name, &r.country, &r.reason, &r.ts); err != nil {
			_ = rows.Close()
			return err
		}
		list = append(list, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	for _, r := range list {
		_, err := d.sql.Exec(`
INSERT INTO node_states(id, address, name, country, status, reason, last_check, updated_at)
VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
  status='ignored',
  country=excluded.country,
  reason=excluded.reason,
  last_check=excluded.last_check,
  updated_at=excluded.updated_at
`, r.id, r.addr, r.name, r.country, StatusIgnored, r.reason, r.ts, r.ts)
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) reload() error {
	rows, err := d.sql.Query(`
SELECT id, address, name, sub_url, country, exit_ip, status, latency_ms, fail_count, reason, last_check
FROM node_states`)
	if err != nil {
		return err
	}
	defer rows.Close()
	m := map[string]NodeState{}
	for rows.Next() {
		var n NodeState
		var latMs, ts int64
		if err := rows.Scan(&n.ID, &n.Address, &n.Name, &n.SubURL, &n.Country, &n.ExitIP,
			&n.Status, &latMs, &n.FailCount, &n.Reason, &ts); err != nil {
			return err
		}
		n.Latency = time.Duration(latMs) * time.Millisecond
		if ts > 0 {
			n.LastCheck = time.Unix(ts, 0)
		}
		m[n.ID] = n
	}
	d.mu.Lock()
	d.cache = m
	d.mu.Unlock()
	return rows.Err()
}

func (d *DB) Get(id string) (NodeState, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	n, ok := d.cache[id]
	return n, ok
}

func (d *DB) CountByStatus(status string) int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	n := 0
	for _, s := range d.cache {
		if s.Status == status {
			n++
		}
	}
	return n
}

func (d *DB) CountIgnored() int { return d.CountByStatus(StatusIgnored) }

func (d *DB) IsIgnored(id string) bool {
	n, ok := d.Get(id)
	return ok && n.Status == StatusIgnored
}

func (d *DB) GetIgnored(id string) (IgnoredNode, bool) {
	n, ok := d.Get(id)
	if !ok || n.Status != StatusIgnored {
		return IgnoredNode{}, false
	}
	return IgnoredNode{
		ID: n.ID, Address: n.Address, Name: n.Name,
		Country: n.Country, Reason: n.Reason, At: n.LastCheck,
	}, true
}

func (d *DB) Save(n NodeState) error {
	if n.ID == "" {
		return fmt.Errorf("empty id")
	}
	if n.Status == "" {
		n.Status = StatusPending
	}
	if n.LastCheck.IsZero() {
		n.LastCheck = time.Now()
	}
	now := time.Now().Unix()
	_, err := d.sql.Exec(`
INSERT INTO node_states(id, address, name, sub_url, country, exit_ip, status, latency_ms, fail_count, reason, last_check, updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
  address=excluded.address,
  name=excluded.name,
  sub_url=excluded.sub_url,
  country=excluded.country,
  exit_ip=excluded.exit_ip,
  status=excluded.status,
  latency_ms=excluded.latency_ms,
  fail_count=excluded.fail_count,
  reason=excluded.reason,
  last_check=excluded.last_check,
  updated_at=excluded.updated_at
`, n.ID, n.Address, n.Name, n.SubURL, n.Country, n.ExitIP, n.Status,
		int64(n.Latency/time.Millisecond), n.FailCount, n.Reason, n.LastCheck.Unix(), now)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.cache[n.ID] = n
	d.mu.Unlock()

	// keep legacy ignored_nodes in sync
	if n.Status == StatusIgnored {
		_, _ = d.sql.Exec(`
INSERT INTO ignored_nodes(id, address, name, country, reason, updated_at)
VALUES(?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
  address=excluded.address, name=excluded.name, country=excluded.country,
  reason=excluded.reason, updated_at=excluded.updated_at
`, n.ID, n.Address, n.Name, n.Country, n.Reason, n.LastCheck.Unix())
	} else {
		_, _ = d.sql.Exec(`DELETE FROM ignored_nodes WHERE id=?`, n.ID)
	}
	return nil
}

func (d *DB) MarkIgnored(n IgnoredNode) error {
	return d.Save(NodeState{
		ID: n.ID, Address: n.Address, Name: n.Name, Country: n.Country,
		Status: StatusIgnored, Reason: n.Reason, LastCheck: n.At,
	})
}

func (d *DB) Unignore(id string) error {
	_, err := d.sql.Exec(`UPDATE node_states SET status=?, updated_at=? WHERE id=? AND status=?`,
		StatusPending, time.Now().Unix(), id, StatusIgnored)
	if err != nil {
		return err
	}
	_, _ = d.sql.Exec(`DELETE FROM ignored_nodes WHERE id=?`, id)
	d.mu.Lock()
	if n, ok := d.cache[id]; ok {
		n.Status = StatusPending
		d.cache[id] = n
	}
	d.mu.Unlock()
	return nil
}

func (d *DB) UnignoreIfAllowed(allow func(code string) bool) (int, error) {
	if allow == nil {
		return 0, nil
	}
	d.mu.RLock()
	var remove []string
	for id, n := range d.cache {
		if n.Status == StatusIgnored && n.Country != "" && allow(n.Country) {
			remove = append(remove, id)
		}
	}
	d.mu.RUnlock()
	for _, id := range remove {
		if err := d.Unignore(id); err != nil {
			return 0, err
		}
	}
	return len(remove), nil
}

// ListByStatus returns cached states with the given status.
func (d *DB) ListByStatus(status string) []NodeState {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var out []NodeState
	for _, n := range d.cache {
		if n.Status == status {
			out = append(out, n)
		}
	}
	return out
}
