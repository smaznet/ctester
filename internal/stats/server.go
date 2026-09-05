package stats

import (
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/aria/x-tester/internal/activity"
	"github.com/aria/x-tester/internal/balancer"
	"github.com/aria/x-tester/internal/config"
	"github.com/aria/x-tester/internal/metrics"
)

type Server struct {
	cfg       config.Stats
	bal       *balancer.Sticky
	dashboard func() Dashboard
	srv       *http.Server
}

// Dashboard is the full live snapshot exposed on /stats for TUI monitor clients.
type Dashboard struct {
	Mode      string            `json:"mode"`
	Balancer  string            `json:"balancer"`
	Listen    string            `json:"listen"`
	StatsAddr string            `json:"stats_addr"`
	Count     int               `json:"count"`
	Active    int               `json:"active"`
	Standby   int               `json:"standby"`
	Failed    int               `json:"failed"`
	Ignored   int               `json:"ignored"`
	Pending   int               `json:"pending"`
	Countries map[string]int    `json:"countries"`
	Filter    []string          `json:"filter"`
	Traffic   TrafficJSON       `json:"traffic"`
	Activity  ActivityJSON      `json:"activity"`
	Logs      []string          `json:"logs"`
	Nodes     []NodeJSON        `json:"nodes"`
}

type NodeJSON struct {
	SubURL    string  `json:"sub_url"`
	Name      string  `json:"name"`
	Tag       string  `json:"tag"`
	Address   string  `json:"address"`
	Country   string  `json:"country"`
	ExitIP    string  `json:"exit_ip"`
	Failed    bool    `json:"failed"`
	Active    bool    `json:"active"`
	Standby   bool    `json:"standby"`
	Ignored   bool    `json:"ignored"`
	LatencyMs float64 `json:"latency_ms"`
	LastCheck string  `json:"last_check"`
}

type TrafficJSON struct {
	BytesUp    uint64  `json:"bytes_up"`
	BytesDown  uint64  `json:"bytes_down"`
	UpRate     float64 `json:"up_rate"`
	DownRate   float64 `json:"down_rate"`
	Conns      int64   `json:"conns"`
	ConnsTotal uint64  `json:"conns_total"`
}

type ActivityJSON struct {
	Phase    string   `json:"phase"`
	Detail   string   `json:"detail"`
	Current  []string `json:"current"`
	Done     int      `json:"done"`
	Total    int      `json:"total"`
	LastOK   string   `json:"last_ok"`
	LastFail string   `json:"last_fail"`
	Line     string   `json:"line"`
}

func New(cfg config.Stats, bal *balancer.Sticky) *Server {
	return &Server{cfg: cfg, bal: bal}
}

// SetDashboard registers the full status provider used by /stats (and monitor).
func (s *Server) SetDashboard(fn func() Dashboard) {
	s.dashboard = fn
}

func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/stats", s.handleStats)
	mux.HandleFunc("/", s.handleStats)
	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	s.srv = &http.Server{Addr: addr, Handler: mux}
	go func() { _ = s.srv.ListenAndServe() }()
	return nil
}

func (s *Server) Close() error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Close()
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	var d Dashboard
	if s.dashboard != nil {
		d = s.dashboard()
	} else {
		d = s.fromBalancerOnly()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(d)
}

func (s *Server) fromBalancerOnly() Dashboard {
	snap := s.bal.Snapshot()
	counts := s.bal.Counts()
	return Dashboard{
		Count:     counts.Total,
		Active:    counts.Active,
		Standby:   counts.Standby,
		Failed:    counts.Failed,
		Ignored:   counts.Ignored,
		Pending:   counts.Pending,
		Countries: s.bal.CountryGroups(),
		Nodes:     NodesFromBalancer(snap),
	}
}

// NodesFromBalancer converts balancer nodes to JSON (hides ignored like the TUI).
func NodesFromBalancer(nodes []balancer.NodeState) []NodeJSON {
	out := make([]NodeJSON, 0, len(nodes))
	for _, n := range nodes {
		if n.Ignored {
			continue
		}
		lc := ""
		if !n.LastCheck.IsZero() {
			lc = n.LastCheck.Format(time.RFC3339)
		}
		out = append(out, NodeJSON{
			SubURL:    n.SubURL,
			Name:      n.Name,
			Tag:       n.ID,
			Address:   n.Address,
			Country:   n.Country,
			ExitIP:    n.ExitIP,
			Failed:    n.Failed,
			Active:    n.Active,
			Standby:   n.Standby,
			Ignored:   n.Ignored,
			LatencyMs: float64(n.Latency) / float64(time.Millisecond),
			LastCheck: lc,
		})
	}
	return out
}

// TrafficFromMetrics maps a metrics snapshot to JSON.
func TrafficFromMetrics(m metrics.Snapshot) TrafficJSON {
	return TrafficJSON{
		BytesUp:    m.BytesUp,
		BytesDown:  m.BytesDown,
		UpRate:     m.UpRate,
		DownRate:   m.DownRate,
		Conns:      m.Conns,
		ConnsTotal: m.ConnsTotal,
	}
}

// ActivityFromTracker maps an activity snapshot to JSON.
func ActivityFromTracker(a activity.Snapshot) ActivityJSON {
	return ActivityJSON{
		Phase:    a.Phase,
		Detail:   a.Detail,
		Current:  a.Current,
		Done:     a.Done,
		Total:    a.Total,
		LastOK:   a.LastOK,
		LastFail: a.LastFail,
		Line:     a.Line,
	}
}
