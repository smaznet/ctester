package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	XrayBin       string    `yaml:"xray_bin"`
	SubURLs       []string  `yaml:"sub_urls"`
	SubRefresh    Duration  `yaml:"sub_refresh"`
	HTTPCheck     HTTPCheck `yaml:"http_check"`
	Balancer      string    `yaml:"balancer"`
	Listen        Listen    `yaml:"listen"`
	Probe         Probe     `yaml:"probe"`
	Stats         Stats     `yaml:"stats"`
	Grouping      Grouping  `yaml:"grouping"`
	FilterCountry []string  `yaml:"filter_country"`
	Database      string    `yaml:"database"` // sqlite path for ignored nodes
}

type Grouping struct {
	Enabled bool   `yaml:"enabled"`
	URL     string `yaml:"url"` // default https://ifconfig.io/country_code
}

type HTTPCheck struct {
	URL            string   `yaml:"url"`
	Method         string   `yaml:"method"`
	ExpectStatus   []int    `yaml:"expect_status"`
	ExpectResponse string   `yaml:"expect_response"`
	Timeout        Duration `yaml:"timeout"`
	// Headers are optional extra HTTP headers on probe requests (connectivity + geo).
	Headers map[string]string `yaml:"headers"`
}

type Listen struct {
	Mode     string   `yaml:"mode"`
	Password string   `yaml:"password"`
	Host     string   `yaml:"host"`
	Port     PortSpec `yaml:"port"`
	Unix     string   `yaml:"unix"`
	// AcceptProxyProtocol requires HAProxy PROXY v1/v2 on each connection.
	// RemoteAddr (hash_ip) becomes the header source IP. Upstream must send the header.
	AcceptProxyProtocol bool `yaml:"accept_proxy_protocol"`
}

type Probe struct {
	Interval       Duration `yaml:"interval"`        // fallback / failed
	IntervalActive Duration `yaml:"interval_active"` // healthy recheck
	IntervalFailed Duration `yaml:"interval_failed"` // previously failed recheck
	Concurrency    int      `yaml:"concurrency"`
	Delay          Duration `yaml:"delay"`
	// MountBatch mounts healthy nodes to main whenever OK count hits a multiple of N.
	// e.g. 10 → mount at 10, 20, 30… healthy. 0 = wait until probe round finishes.
	MountBatch int `yaml:"mount_batch"`
	// MaxActive caps mounted healthy nodes. 0 = unlimited.
	// At cap: fill probes pause unless Standby still needs warm spares.
	MaxActive int `yaml:"max_active"`
	// Standby is how many extra healthy (probed, not mounted) nodes to keep ready.
	// When an active drops, these mount immediately without waiting for a new probe.
	// Requires MaxActive > 0. 0 = disabled.
	Standby int `yaml:"standby"`
	// LatencyTolerance: if an active's latency exceeds the best active by more than this,
	// it is unmounted and marked failed (not standby). 0 = disabled.
	// A later probe with a competitive latency can bring it back into the pool.
	LatencyTolerance Duration `yaml:"latency_tolerance"`
}

type Stats struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("duration must be scalar")
	}
	raw := strings.TrimSpace(value.Value)
	if raw == "" {
		*d = 0
		return nil
	}
	if secs, err := strconv.Atoi(raw); err == nil {
		*d = Duration(time.Duration(secs) * time.Second)
		return nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) Std() time.Duration { return time.Duration(d) }

// PortSpec is a single port or inclusive range "1080-1090".
type PortSpec struct {
	Start int
	End   int
}

func (p *PortSpec) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("port must be scalar")
	}
	raw := strings.TrimSpace(value.Value)
	if raw == "" {
		return fmt.Errorf("port is empty")
	}
	if strings.Contains(raw, "-") {
		parts := strings.SplitN(raw, "-", 2)
		start, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		end, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err1 != nil || err2 != nil || start <= 0 || end < start {
			return fmt.Errorf("invalid port range %q", raw)
		}
		p.Start, p.End = start, end
		return nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fmt.Errorf("invalid port %q", raw)
	}
	p.Start, p.End = n, n
	return nil
}

func (p PortSpec) Ports() []int {
	out := make([]int, 0, p.End-p.Start+1)
	for i := p.Start; i <= p.End; i++ {
		out = append(out, i)
	}
	return out
}

func (p PortSpec) IsRange() bool { return p.End > p.Start }

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.XrayBin == "" {
		c.XrayBin = "xray"
	}
	if c.SubRefresh == 0 {
		c.SubRefresh = Duration(5 * time.Minute)
	}
	if c.HTTPCheck.Method == "" {
		c.HTTPCheck.Method = "GET"
	}
	if c.HTTPCheck.URL == "" {
		c.HTTPCheck.URL = "https://www.gstatic.com/generate_204"
	}
	if len(c.HTTPCheck.ExpectStatus) == 0 {
		c.HTTPCheck.ExpectStatus = []int{204, 200}
	}
	if c.HTTPCheck.Timeout == 0 {
		c.HTTPCheck.Timeout = Duration(5 * time.Second)
	}
	if c.Balancer == "" {
		c.Balancer = "hash_username"
	}
	if c.Listen.Mode == "" {
		c.Listen.Mode = "socks5"
	}
	if c.Listen.Host == "" {
		c.Listen.Host = "127.0.0.1"
	}
	if c.Probe.Interval == 0 {
		c.Probe.Interval = Duration(30 * time.Second)
	}
	if c.Probe.IntervalFailed == 0 {
		c.Probe.IntervalFailed = c.Probe.Interval
	}
	if c.Probe.IntervalActive == 0 {
		c.Probe.IntervalActive = Duration(5 * time.Minute)
	}
	if c.Probe.Concurrency <= 0 {
		c.Probe.Concurrency = 10
	}
	if c.Probe.Delay == 0 {
		c.Probe.Delay = Duration(time.Second)
	}
	// mount_batch: 0 = فقط آخر راند؛ منفی = پیش‌فرض ۱۰
	if c.Probe.MountBatch < 0 {
		c.Probe.MountBatch = 10
	}
	if c.Probe.MaxActive < 0 {
		c.Probe.MaxActive = 0
	}
	if c.Probe.Standby < 0 {
		c.Probe.Standby = 0
	}
	if c.Probe.MaxActive == 0 {
		c.Probe.Standby = 0 // standby only makes sense with an active cap
	}
	if c.Grouping.URL == "" {
		c.Grouping.URL = "https://ifconfig.io/country_code"
	}
	if c.Database == "" {
		c.Database = "x-tester.db"
	}
	// normalize country filter to upper-case codes
	if len(c.FilterCountry) > 0 {
		out := make([]string, 0, len(c.FilterCountry))
		for _, x := range c.FilterCountry {
			x = strings.ToUpper(strings.TrimSpace(x))
			if x != "" {
				out = append(out, x)
			}
		}
		c.FilterCountry = out
	}
	if c.Stats.Host == "" {
		c.Stats.Host = "127.0.0.1"
	}
	if c.Stats.Port == 0 {
		c.Stats.Port = 9090
	}
}

func (c *Config) Validate() error {
	if len(c.SubURLs) == 0 {
		return fmt.Errorf("sub_urls is required")
	}
	switch c.Balancer {
	case "hash_username", "hash_ip", "in_port":
	default:
		return fmt.Errorf("invalid balancer %q", c.Balancer)
	}
	switch c.Listen.Mode {
	case "socks5", "http", "direct":
	default:
		return fmt.Errorf("invalid listen.mode %q", c.Listen.Mode)
	}
	if c.Listen.Unix == "" && c.Listen.Port.Start == 0 {
		return fmt.Errorf("listen.port or listen.unix is required")
	}
	if c.Listen.Mode != "direct" && c.Listen.Password == "" {
		return fmt.Errorf("listen.password is required for socks5/http")
	}
	if c.Balancer == "hash_username" && c.Listen.Mode == "direct" {
		return fmt.Errorf("hash_username requires socks5/http mode")
	}
	if c.Balancer == "in_port" && c.Listen.Unix != "" {
		return fmt.Errorf("in_port balancer cannot use unix socket")
	}
	return nil
}

// CountryAllowed returns true if filter is empty or code is listed.
func (c *Config) CountryAllowed(code string) bool {
	if len(c.FilterCountry) == 0 {
		return true
	}
	code = strings.ToUpper(strings.TrimSpace(code))
	for _, x := range c.FilterCountry {
		if x == code {
			return true
		}
	}
	return false
}

// NeedGeo is true when country must be detected.
func (c *Config) NeedGeo() bool {
	return c.Grouping.Enabled || len(c.FilterCountry) > 0
}
