package activity

import (
	"fmt"
	"strings"
	"sync"
)

// Tracker holds the current high-level job and in-flight probe names.
type Tracker struct {
	mu      sync.RWMutex
	phase   string
	detail  string
	current map[string]string // id -> display name
	done    int
	total   int
	lastOK  string
	lastFail string
}

func New() *Tracker {
	return &Tracker{
		phase:   "starting",
		current: map[string]string{},
	}
}

func (t *Tracker) Set(phase, detail string) {
	t.mu.Lock()
	t.phase = phase
	t.detail = detail
	t.mu.Unlock()
}

func (t *Tracker) BeginProbe(total int) {
	t.mu.Lock()
	t.phase = "probing"
	t.detail = fmt.Sprintf("0/%d", total)
	t.done = 0
	t.total = total
	t.current = map[string]string{}
	t.mu.Unlock()
}

func (t *Tracker) StartNode(id, name string) {
	t.mu.Lock()
	t.current[id] = name
	t.detail = fmt.Sprintf("%d/%d testing", t.done, t.total)
	t.mu.Unlock()
}

func (t *Tracker) FinishNode(id, name string, ok bool, latency, errMsg string) {
	t.mu.Lock()
	delete(t.current, id)
	t.done++
	t.detail = fmt.Sprintf("%d/%d", t.done, t.total)
	if ok {
		t.lastOK = fmt.Sprintf("OK %s %s", name, latency)
	} else {
		if errMsg == "" {
			errMsg = "fail"
		}
		t.lastFail = fmt.Sprintf("FAIL %s %s", name, errMsg)
	}
	t.mu.Unlock()
}

func (t *Tracker) Idle(msg string) {
	t.mu.Lock()
	t.phase = "idle"
	t.detail = msg
	t.current = map[string]string{}
	t.mu.Unlock()
}

type Snapshot struct {
	Phase    string
	Detail   string
	Current  []string
	Done     int
	Total    int
	LastOK   string
	LastFail string
	Line     string
}

func (t *Tracker) Snapshot() Snapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	cur := make([]string, 0, len(t.current))
	for _, name := range t.current {
		cur = append(cur, name)
	}
	s := Snapshot{
		Phase:    t.phase,
		Detail:   t.detail,
		Current:  cur,
		Done:     t.done,
		Total:    t.total,
		LastOK:   t.lastOK,
		LastFail: t.lastFail,
	}
	var b strings.Builder
	b.WriteString(t.phase)
	if t.detail != "" {
		b.WriteString(" · ")
		b.WriteString(t.detail)
	}
	if len(cur) > 0 {
		b.WriteString(" · now: ")
		shown := cur
		if len(shown) > 3 {
			shown = shown[:3]
		}
		b.WriteString(strings.Join(shown, ", "))
		if len(cur) > 3 {
			b.WriteString(fmt.Sprintf(" +%d", len(cur)-3))
		}
	}
	s.Line = b.String()
	return s
}
