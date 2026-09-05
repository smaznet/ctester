package monitor

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/aria/x-tester/internal/activity"
	"github.com/aria/x-tester/internal/balancer"
	"github.com/aria/x-tester/internal/config"
	"github.com/aria/x-tester/internal/metrics"
	"github.com/aria/x-tester/internal/stats"
	"github.com/aria/x-tester/internal/tui"
)

// Options for Run.
type Options struct {
	Addr       string // host:port of running instance stats server
	ConfigPath string // optional; used to resolve Addr from stats.host/port
}

// Run attaches a local TUI to a remote (usually systemd) x-tester via /stats.
func Run(opts Options) error {
	addr, err := resolveAddr(opts)
	if err != nil {
		return err
	}
	base := "http://" + addr

	var (
		mu     sync.Mutex
		last   tui.Status
		errMsg string
	)
	last = tui.Status{
		StatsAddr: addr,
		Remote:    true,
		Activity:  activity.Snapshot{Line: "connecting to " + addr + "…"},
	}

	client := &http.Client{Timeout: 2 * time.Second}
	fetch := func() {
		st, err := fetchStatus(client, base+"/stats")
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			errMsg = err.Error()
			last.Remote = true
			last.StatsAddr = addr
			last.Activity = activity.Snapshot{
				Phase: "offline",
				Line:  "offline · " + errMsg,
			}
			last.Logs = []string{"monitor: " + errMsg}
			return
		}
		errMsg = ""
		last = st
	}
	fetch()

	prog := tui.New(func() tui.Status {
		mu.Lock()
		defer mu.Unlock()
		return last
	}, nil)

	// refresh a bit slower than in-process TUI; HTTP poll
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for range t.C {
			fetch()
		}
	}()

	if _, err := prog.Run(); err != nil {
		return err
	}
	return nil
}

func resolveAddr(opts Options) (string, error) {
	if opts.Addr != "" {
		host, port, err := net.SplitHostPort(opts.Addr)
		if err != nil {
			return "", fmt.Errorf("invalid -addr %q: %w", opts.Addr, err)
		}
		if host == "" {
			host = "127.0.0.1"
		}
		return net.JoinHostPort(host, port), nil
	}
	cfgPath := opts.ConfigPath
	if cfgPath == "" {
		cfgPath = "config.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil && cfgPath == "config.yaml" {
		cfg, err = config.Load("/etc/x-tester/config.yaml")
	}
	if err != nil {
		// fall back to default stats bind
		return net.JoinHostPort("127.0.0.1", "9090"), nil
	}
	host := cfg.Stats.Host
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	port := cfg.Stats.Port
	if port == 0 {
		port = 9090
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func fetchStatus(client *http.Client, url string) (tui.Status, error) {
	resp, err := client.Get(url)
	if err != nil {
		return tui.Status{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return tui.Status{}, fmt.Errorf("stats %s", resp.Status)
	}
	var d stats.Dashboard
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return tui.Status{}, err
	}
	return dashboardToStatus(d), nil
}

func dashboardToStatus(d stats.Dashboard) tui.Status {
	nodes := make([]balancer.NodeState, 0, len(d.Nodes))
	active := make([]balancer.NodeState, 0)
	rest := make([]balancer.NodeState, 0)
	for _, n := range d.Nodes {
		st := balancer.NodeState{
			ID:      n.Tag,
			Name:    n.Name,
			SubURL:  n.SubURL,
			Address: n.Address,
			Country: n.Country,
			ExitIP:  n.ExitIP,
			Failed:  n.Failed,
			Active:  n.Active,
			Standby: n.Standby,
			Ignored: n.Ignored,
			Latency: time.Duration(n.LatencyMs * float64(time.Millisecond)),
		}
		if n.LastCheck != "" {
			if t, err := time.Parse(time.RFC3339, n.LastCheck); err == nil {
				st.LastCheck = t
			}
		}
		if st.Active && !st.Failed {
			active = append(active, st)
		} else {
			rest = append(rest, st)
		}
	}
	nodes = append(active, rest...)

	return tui.Status{
		Mode:       d.Mode,
		Balancer:   d.Balancer,
		Listen:     d.Listen,
		StatsAddr:  d.StatsAddr,
		TotalNodes: d.Count,
		Active:     d.Active,
		Standby:    d.Standby,
		Failed:     d.Failed,
		Ignored:    d.Ignored,
		Pending:    d.Pending,
		Countries:  d.Countries,
		Filter:     d.Filter,
		Traffic: metrics.Snapshot{
			BytesUp:    d.Traffic.BytesUp,
			BytesDown:  d.Traffic.BytesDown,
			UpRate:     d.Traffic.UpRate,
			DownRate:   d.Traffic.DownRate,
			Conns:      d.Traffic.Conns,
			ConnsTotal: d.Traffic.ConnsTotal,
		},
		Nodes: nodes,
		Logs:  d.Logs,
		Activity: activity.Snapshot{
			Phase:    d.Activity.Phase,
			Detail:   d.Activity.Detail,
			Current:  d.Activity.Current,
			Done:     d.Activity.Done,
			Total:    d.Activity.Total,
			LastOK:   d.Activity.LastOK,
			LastFail: d.Activity.LastFail,
			Line:     d.Activity.Line,
		},
		Remote: true,
	}
}
