package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aria/x-tester/internal/config"
	"github.com/aria/x-tester/internal/sub"
	"github.com/aria/x-tester/internal/xray"
	"golang.org/x/net/proxy"
)

type Result struct {
	Node    sub.Node
	OK      bool
	Ignored bool // country filtered — do not retest
	Country string
	ExitIP  string
	Latency time.Duration
	Local   *xray.NodeLocal
	Error   string
}

type Checker struct {
	Probe        *xray.Instance
	Check        config.HTTPCheck
	Timeout      time.Duration
	NeedGeo      bool
	GeoURL       string
	AllowCountry func(code string) bool
}

func (c *Checker) CheckNode(ctx context.Context, node sub.Node) Result {
	tag := "p-" + node.ID
	local, err := c.Probe.AddNode(ctx, tag, node.Outbound)
	if err != nil {
		return Result{Node: node, OK: false, Error: err.Error()}
	}
	defer func() {
		_ = c.Probe.RemoveNode(context.Background(), local.OutboundTag, local.InboundTag)
	}()

	start := time.Now()

	// connectivity check (optional if geo-only — still run when configured)
	if c.Check.URL != "" && (!c.NeedGeo || c.Check.URL != c.GeoURL) {
		if err := httpThroughSocks(ctx, local.LocalPort, c.Check); err != nil {
			return Result{Node: node, OK: false, Latency: time.Since(start), Error: err.Error()}
		}
	}

	country, exitIP := "", ""
	if c.NeedGeo {
		geoURL := c.GeoURL
		if geoURL == "" {
			geoURL = "https://ifconfig.io/country_code"
		}
		info, err := geoThroughSocks(ctx, local.LocalPort, geoURL, c.Timeout, c.Check.Headers)
		if err != nil {
			return Result{Node: node, OK: false, Latency: time.Since(start), Error: "geo: " + err.Error()}
		}
		country = strings.ToUpper(strings.TrimSpace(info.Country))
		exitIP = info.IP
		if country == "" {
			return Result{Node: node, OK: false, Latency: time.Since(start), Error: "geo: empty country"}
		}
		if c.AllowCountry != nil && !c.AllowCountry(country) {
			return Result{
				Node:    node,
				OK:      false,
				Ignored: true,
				Country: country,
				ExitIP:  exitIP,
				Latency: time.Since(start),
				Error:   "country filtered: " + country,
			}
		}
	}

	return Result{
		Node:    node,
		OK:      true,
		Country: country,
		ExitIP:  exitIP,
		Latency: time.Since(start),
	}
}

type geoInfo struct {
	Country string
	IP      string
}

func geoThroughSocks(ctx context.Context, socksPort int, geoURL string, timeout time.Duration, headers map[string]string) (geoInfo, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	check := config.HTTPCheck{
		URL:          geoURL,
		Method:       http.MethodGet,
		ExpectStatus: []int{200},
		Timeout:      config.Duration(timeout),
		Headers:      headers,
	}
	body, err := httpBodyThroughSocks(ctx, socksPort, check)
	if err != nil {
		return geoInfo{}, err
	}
	raw := strings.TrimSpace(string(body))
	if raw == "" {
		return geoInfo{}, fmt.Errorf("empty body")
	}
	// Plain text: "US" (ifconfig.io/country_code)
	if !strings.HasPrefix(raw, "{") {
		code := strings.ToUpper(strings.Fields(raw)[0])
		if len(code) < 2 || len(code) > 3 {
			return geoInfo{}, fmt.Errorf("bad country_code %q", raw)
		}
		return geoInfo{Country: code}, nil
	}
	// JSON fallback (all.json etc.)
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return geoInfo{}, fmt.Errorf("json: %w", err)
	}
	country := strField(m, "country_code", "country", "CountryCode", "countryCode")
	ip := strField(m, "ip", "IP", "query")
	return geoInfo{Country: country, IP: ip}, nil
}

func strField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch t := v.(type) {
			case string:
				return t
			}
		}
	}
	return ""
}

func httpThroughSocks(ctx context.Context, socksPort int, check config.HTTPCheck) error {
	_, err := httpBodyThroughSocks(ctx, socksPort, check)
	return err
}

func httpBodyThroughSocks(ctx context.Context, socksPort int, check config.HTTPCheck) ([]byte, error) {
	dialer, err := proxy.SOCKS5("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", socksPort)), nil, proxy.Direct)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		},
		DisableKeepAlives: true,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   check.Timeout.Std(),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	method := check.Method
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, check.URL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range check.Headers {
		if k == "" {
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if !statusOK(resp.StatusCode, check.ExpectStatus) {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	if check.ExpectResponse != "" && !strings.Contains(string(body), check.ExpectResponse) {
		return nil, fmt.Errorf("response mismatch")
	}
	if err := headersOK(resp.Header, check.ExpectHeaders); err != nil {
		return nil, err
	}
	return body, nil
}

// headersOK checks that each expected header value contains the given substring.
func headersOK(h http.Header, expect map[string]string) error {
	for name, want := range expect {
		if name == "" || want == "" {
			continue
		}
		got := h.Get(name)
		if got == "" {
			return fmt.Errorf("header %q missing", name)
		}
		if !strings.Contains(got, want) {
			return fmt.Errorf("header %q mismatch", name)
		}
	}
	return nil
}

func statusOK(code int, expect []int) bool {
	if len(expect) == 0 {
		return code >= 200 && code < 400
	}
	for _, e := range expect {
		if code == e {
			return true
		}
	}
	return false
}

type Pool struct {
	Concurrency int
	Delay       time.Duration
	Checker     *Checker
	OnStart     func(node sub.Node)
	OnResult    func(Result)
	// ShouldSkip, if set, skips testing a node (no OnStart/OnResult). Used for max_active cap.
	ShouldSkip func(node sub.Node) bool
}

func (p *Pool) Run(ctx context.Context, nodes []sub.Node) []Result {
	if p.Concurrency <= 0 {
		p.Concurrency = 10
	}
	ch := make(chan sub.Node)
	out := make(chan Result, len(nodes))
	var wg sync.WaitGroup
	for i := 0; i < p.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range ch {
				select {
				case <-ctx.Done():
					return
				default:
				}
				if p.ShouldSkip != nil && p.ShouldSkip(n) {
					continue
				}
				if p.OnStart != nil {
					p.OnStart(n)
				}
				r := p.Checker.CheckNode(ctx, n)
				if p.OnResult != nil {
					p.OnResult(r)
				}
				out <- r
				if p.Delay > 0 {
					timer := time.NewTimer(p.Delay)
					select {
					case <-ctx.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
				}
			}
		}()
	}
	go func() {
		for _, n := range nodes {
			select {
			case <-ctx.Done():
				close(ch)
				return
			case ch <- n:
			}
		}
		close(ch)
	}()
	go func() {
		wg.Wait()
		close(out)
	}()
	var results []Result
	for r := range out {
		results = append(results, r)
	}
	return results
}

func DialViaSocks(socksPort int, network, address string) (net.Conn, error) {
	dialer, err := proxy.SOCKS5("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", socksPort)), nil, proxy.Direct)
	if err != nil {
		return nil, err
	}
	return dialer.Dial(network, address)
}

func SocksURL(port int) *url.URL {
	return &url.URL{Scheme: "socks5", Host: net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", port))}
}
