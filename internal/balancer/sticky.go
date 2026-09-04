package balancer

import (
	"hash/fnv"
	"sort"
	"sync"
	"time"
)

type NodeState struct {
	ID        string
	Name      string
	SubURL    string
	Address   string
	LocalPort int
	Active    bool
	Failed    bool
	Ignored   bool // filtered out by country — never retest
	Standby   bool // probed OK, kept warm (not mounted) for fast replace
	Country   string
	ExitIP    string
	Latency   time.Duration
	LastCheck time.Time
	FailCount int
}

type Sticky struct {
	mu       sync.RWMutex
	mode     string
	nodes    map[string]*NodeState // id -> state
	primary  map[string]string     // stickyKey -> preferred node id
	current  map[string]string     // stickyKey -> currently used node id
}

func New(mode string) *Sticky {
	return &Sticky{
		mode:    mode,
		nodes:   map[string]*NodeState{},
		primary: map[string]string{},
		current: map[string]string{},
	}
}

func (s *Sticky) Mode() string { return s.mode }

func (s *Sticky) SetMode(mode string) {
	s.mu.Lock()
	s.mode = mode
	s.mu.Unlock()
}

func (s *Sticky) Upsert(n NodeState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := n
	s.nodes[n.ID] = &cp
}

func (s *Sticky) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.nodes, id)
	for k, v := range s.primary {
		if v == id {
			delete(s.primary, k)
		}
	}
	for k, v := range s.current {
		if v == id {
			delete(s.current, k)
		}
	}
}

func (s *Sticky) Snapshot() []NodeState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]NodeState, 0, len(s.nodes))
	for _, n := range s.nodes {
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Sticky) Active() []NodeState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []NodeState
	for _, n := range s.nodes {
		if n.Active && !n.Failed && n.LocalPort > 0 {
			out = append(out, *n)
		}
	}
	return out
}

// UpsertPending inserts node if missing; does not downgrade an active/failed/ignored entry.
func (s *Sticky) UpsertPending(n NodeState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.nodes[n.ID]; ok {
		cur.Name = n.Name
		cur.SubURL = n.SubURL
		cur.Address = n.Address
		return
	}
	cp := n
	s.nodes[n.ID] = &cp
}

func (s *Sticky) Get(id string) (NodeState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n, ok := s.nodes[id]
	if !ok {
		return NodeState{}, false
	}
	return *n, true
}

type Counts struct {
	Total   int
	Active  int
	Failed  int
	Ignored int
	Pending int
	Standby int
}

func (s *Sticky) Counts() Counts {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var c Counts
	c.Total = len(s.nodes)
	for _, n := range s.nodes {
		switch {
		case n.Ignored:
			c.Ignored++
		case n.Active && !n.Failed && n.LocalPort > 0:
			c.Active++
		case n.Standby && !n.Failed && !n.Ignored:
			c.Standby++
		case n.Failed:
			c.Failed++
		default:
			c.Pending++
		}
	}
	return c
}

// CountryGroups returns counts per country code (non-ignored with known country).
func (s *Sticky) CountryGroups() map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]int{}
	for _, n := range s.nodes {
		if n.Country == "" || n.Ignored {
			continue
		}
		out[n.Country]++
	}
	return out
}

// Pick returns local socks port for the sticky key.
func (s *Sticky) Pick(username, clientIP string, inPort int) (localPort int, nodeID string, ok bool) {
	key := s.key(username, clientIP, inPort)
	s.mu.Lock()
	defer s.mu.Unlock()

	active := s.activeLocked()
	if len(active) == 0 {
		return 0, "", false
	}

	// Restore primary if it is healthy again.
	if pid, exists := s.primary[key]; exists {
		if n := s.nodes[pid]; n != nil && n.Active && !n.Failed && n.LocalPort > 0 {
			s.current[key] = pid
			return n.LocalPort, pid, true
		}
	}

	// Keep current failover target if still healthy.
	if cid, exists := s.current[key]; exists {
		if n := s.nodes[cid]; n != nil && n.Active && !n.Failed && n.LocalPort > 0 {
			return n.LocalPort, cid, true
		}
	}

	chosen := weightedPick(active, key)
	if chosen == nil {
		return 0, "", false
	}
	if _, hasPrimary := s.primary[key]; !hasPrimary {
		s.primary[key] = chosen.ID
	}
	s.current[key] = chosen.ID
	return chosen.LocalPort, chosen.ID, true
}

func (s *Sticky) key(username, clientIP string, inPort int) string {
	switch s.mode {
	case "hash_ip":
		return "ip:" + clientIP
	case "in_port":
		return "port:" + itoa(inPort)
	default:
		return "user:" + username
	}
}

func (s *Sticky) activeLocked() []*NodeState {
	var out []*NodeState
	for _, n := range s.nodes {
		if n.Active && !n.Failed && n.LocalPort > 0 {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// weightedPick: lower latency => higher weight. Sticky-ish via hash into weighted ring.
func weightedPick(nodes []*NodeState, key string) *NodeState {
	if len(nodes) == 0 {
		return nil
	}
	type item struct {
		n      *NodeState
		weight int
	}
	var items []item
	total := 0
	for _, n := range nodes {
		w := latencyWeight(n.Latency)
		items = append(items, item{n: n, weight: w})
		total += w
	}
	if total <= 0 {
		return nodes[hash(key)%uint32(len(nodes))]
	}
	h := hash(key) % uint32(total)
	var acc uint32
	for _, it := range items {
		acc += uint32(it.weight)
		if h < acc {
			return it.n
		}
	}
	return items[len(items)-1].n
}

func latencyWeight(d time.Duration) int {
	if d <= 0 {
		return 1
	}
	ms := int(d / time.Millisecond)
	if ms < 1 {
		ms = 1
	}
	// weight ≈ 10000 / ms, clamped
	w := 10000 / ms
	if w < 1 {
		w = 1
	}
	if w > 5000 {
		w = 5000
	}
	return w
}

func hash(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
