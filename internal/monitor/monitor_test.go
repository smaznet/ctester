package monitor

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aria/x-tester/internal/stats"
)

func TestDashboardToStatus(t *testing.T) {
	d := stats.Dashboard{
		Mode:     "socks5",
		Balancer: "hash_ip",
		Listen:   "127.0.0.1:1080",
		Count:    2,
		Active:   1,
		Failed:   1,
		Traffic:  stats.TrafficJSON{Conns: 3, DownRate: 1024},
		Activity: stats.ActivityJSON{Line: "idle · ok"},
		Logs:     []string{"hello"},
		Nodes: []stats.NodeJSON{
			{Tag: "a", Name: "ok", Active: true, LatencyMs: 12.5},
			{Tag: "b", Name: "bad", Failed: true, LatencyMs: 90},
		},
	}
	st := dashboardToStatus(d)
	if !st.Remote || st.Mode != "socks5" || st.Active != 1 || len(st.Nodes) != 2 {
		t.Fatalf("bad status: %+v", st)
	}
	if st.Nodes[0].Name != "ok" || st.Nodes[0].Latency != 12*time.Millisecond+500*time.Microsecond {
		// 12.5ms
		if st.Nodes[0].Latency < 12*time.Millisecond {
			t.Fatalf("latency: %v", st.Nodes[0].Latency)
		}
	}
	if st.Traffic.Conns != 3 || st.Logs[0] != "hello" {
		t.Fatalf("traffic/logs: %+v", st)
	}
}

func TestFetchStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(stats.Dashboard{
			Mode: "direct", Active: 2, Count: 2,
			Activity: stats.ActivityJSON{Line: "probing"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	st, err := fetchStatus(srv.Client(), srv.URL+"/stats")
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode != "direct" || st.Active != 2 || !st.Remote {
		t.Fatalf("%+v", st)
	}
}

func TestResolveAddrDefault(t *testing.T) {
	addr, err := resolveAddr(Options{Addr: ":9090"})
	if err != nil {
		t.Fatal(err)
	}
	if addr != "127.0.0.1:9090" {
		t.Fatalf("got %q", addr)
	}
}
