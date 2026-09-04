package stats

import (
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/aria/x-tester/internal/balancer"
	"github.com/aria/x-tester/internal/config"
)

type Server struct {
	cfg config.Stats
	bal *balancer.Sticky
	srv *http.Server
}

type nodeJSON struct {
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

func New(cfg config.Stats, bal *balancer.Sticky) *Server {
	return &Server{cfg: cfg, bal: bal}
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
	snap := s.bal.Snapshot()
	counts := s.bal.Counts()
	out := make([]nodeJSON, 0, len(snap))
	for _, n := range snap {
		out = append(out, nodeJSON{
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
			LastCheck: n.LastCheck.Format(time.RFC3339),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"count":     counts.Total,
		"active":    counts.Active,
		"standby":   counts.Standby,
		"failed":    counts.Failed,
		"ignored":   counts.Ignored,
		"pending":   counts.Pending,
		"countries": s.bal.CountryGroups(),
		"nodes":     out,
	})
}
