package sub

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Node struct {
	ID       string
	Name     string
	SubURL   string
	Protocol string
	Address  string
	Port     int
	Outbound map[string]any
}

func (n Node) AddressPort() string {
	return net.JoinHostPort(n.Address, strconv.Itoa(n.Port))
}

func FetchAll(urls []string, timeout time.Duration) ([]Node, []error) {
	client := &http.Client{Timeout: timeout}
	var all []Node
	var errs []error
	seen := map[string]struct{}{}
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		nodes, err := fetchOne(client, u)
		if err != nil {
			errs = append(errs, fmt.Errorf("fetch %s: %w", u, err))
			continue
		}
		for _, n := range nodes {
			n.SubURL = u
			if _, ok := seen[n.ID]; ok {
				continue
			}
			seen[n.ID] = struct{}{}
			all = append(all, n)
		}
	}
	Shuffle(all)
	return all, errs
}

// Shuffle randomizes node order in place (probe / remount fairness).
func Shuffle(nodes []Node) {
	rand.Shuffle(len(nodes), func(i, j int) {
		nodes[i], nodes[j] = nodes[j], nodes[i]
	})
}

func fetchOne(client *http.Client, rawURL string) ([]Node, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "x-tester/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	return ParseSubscription(string(body))
}

func ParseSubscription(body string) ([]Node, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, nil
	}
	text := body
	if decoded, err := decodeBase64(body); err == nil && looksLikeLinkList(decoded) {
		text = decoded
	}
	var nodes []Node
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		n, err := ParseLink(line)
		if err != nil {
			continue
		}
		nodes = append(nodes, n)
	}
	return nodes, nil
}

func looksLikeLinkList(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "vmess://") ||
		strings.Contains(s, "vless://") ||
		strings.Contains(s, "trojan://") ||
		strings.Contains(s, "ss://")
}

func decodeBase64(s string) (string, error) {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.ReplaceAll(s, "\r", "")
	encodings := []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	}
	for _, enc := range encodings {
		b, err := enc.DecodeString(s)
		if err == nil && len(b) > 0 {
			return string(b), nil
		}
	}
	return "", fmt.Errorf("not base64")
}

func ParseLink(link string) (Node, error) {
	switch {
	case strings.HasPrefix(link, "vmess://"):
		return parseVMess(link)
	case strings.HasPrefix(link, "vless://"):
		return parseVLESS(link)
	case strings.HasPrefix(link, "trojan://"):
		return parseTrojan(link)
	case strings.HasPrefix(link, "ss://"):
		return parseShadowsocks(link)
	default:
		return Node{}, fmt.Errorf("unsupported link")
	}
}

func parseVMess(link string) (Node, error) {
	raw := strings.TrimPrefix(link, "vmess://")
	decoded, err := decodeBase64(raw)
	if err != nil {
		return Node{}, err
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(decoded), &m); err != nil {
		return Node{}, err
	}
	host := str(m["add"])
	port := anyInt(m["port"])
	uuid := str(m["id"])
	aid := anyInt(m["aid"])
	net := strOr(m["net"], "tcp")
	typ := strOr(m["type"], "none")
	hostHeader := str(m["host"])
	path := str(m["path"])
	tls := str(m["tls"])
	sni := strOr(m["sni"], hostHeader)
	name := strOr(m["ps"], host)

	stream := map[string]any{
		"network": net,
	}
	switch net {
	case "ws":
		ws := map[string]any{"path": path}
		if hostHeader != "" {
			ws["headers"] = map[string]any{"Host": hostHeader}
		}
		stream["wsSettings"] = ws
	case "grpc":
		stream["grpcSettings"] = map[string]any{"serviceName": path}
	case "tcp":
		if typ == "http" {
			stream["tcpSettings"] = map[string]any{
				"header": map[string]any{
					"type": "http",
					"request": map[string]any{
						"path": []string{pathOr(path, "/")},
						"headers": map[string]any{
							"Host": []string{hostHeader},
						},
					},
				},
			}
		}
	}
	if tls == "tls" {
		stream["security"] = "tls"
		tlsSettings := map[string]any{}
		if sni != "" {
			tlsSettings["serverName"] = sni
		}
		stream["tlsSettings"] = tlsSettings
	}

	outbound := map[string]any{
		"protocol": "vmess",
		"settings": map[string]any{
			"vnext": []any{
				map[string]any{
					"address": host,
					"port":    port,
					"users": []any{
						map[string]any{
							"id":       uuid,
							"alterId":  aid,
							"security": strOr(m["scy"], "auto"),
						},
					},
				},
			},
		},
		"streamSettings": stream,
	}
	return makeNode("vmess", name, host, port, outbound), nil
}

func parseVLESS(link string) (Node, error) {
	u, err := url.Parse(link)
	if err != nil {
		return Node{}, err
	}
	host := u.Hostname()
	port, _ := strconv.Atoi(u.Port())
	if port == 0 {
		port = 443
	}
	q := u.Query()
	uuid := u.User.Username()
	name, _ := url.QueryUnescape(strings.TrimPrefix(u.Fragment, ""))
	if name == "" {
		name = host
	}
	network := q.Get("type")
	if network == "" {
		network = "tcp"
	}
	security := q.Get("security")
	stream := map[string]any{"network": network}
	switch network {
	case "ws":
		ws := map[string]any{"path": q.Get("path")}
		if h := q.Get("host"); h != "" {
			ws["headers"] = map[string]any{"Host": h}
		}
		stream["wsSettings"] = ws
	case "grpc":
		stream["grpcSettings"] = map[string]any{
			"serviceName": q.Get("serviceName"),
		}
	case "tcp":
		if q.Get("headerType") == "http" {
			stream["tcpSettings"] = map[string]any{
				"header": map[string]any{"type": "http"},
			}
		}
	}
	switch security {
	case "tls":
		stream["security"] = "tls"
		tlsSettings := map[string]any{}
		if sni := q.Get("sni"); sni != "" {
			tlsSettings["serverName"] = sni
		}
		if fp := q.Get("fp"); fp != "" {
			tlsSettings["fingerprint"] = fp
		}
		if alpn := q.Get("alpn"); alpn != "" {
			tlsSettings["alpn"] = strings.Split(alpn, ",")
		}
		stream["tlsSettings"] = tlsSettings
	case "reality":
		stream["security"] = "reality"
		reality := map[string]any{
			"serverName":  q.Get("sni"),
			"fingerprint": strOrVal(q.Get("fp"), "chrome"),
			"publicKey":   q.Get("pbk"),
			"shortId":     q.Get("sid"),
			"spiderX":     q.Get("spx"),
		}
		stream["realitySettings"] = reality
	}

	user := map[string]any{
		"id":         uuid,
		"encryption": strOrVal(q.Get("encryption"), "none"),
	}
	if flow := q.Get("flow"); flow != "" {
		user["flow"] = flow
	}
	outbound := map[string]any{
		"protocol": "vless",
		"settings": map[string]any{
			"vnext": []any{
				map[string]any{
					"address": host,
					"port":    port,
					"users":   []any{user},
				},
			},
		},
		"streamSettings": stream,
	}
	return makeNode("vless", name, host, port, outbound), nil
}

func parseTrojan(link string) (Node, error) {
	u, err := url.Parse(link)
	if err != nil {
		return Node{}, err
	}
	host := u.Hostname()
	port, _ := strconv.Atoi(u.Port())
	if port == 0 {
		port = 443
	}
	password, _ := url.QueryUnescape(u.User.Username())
	q := u.Query()
	name, _ := url.QueryUnescape(u.Fragment)
	if name == "" {
		name = host
	}
	network := q.Get("type")
	if network == "" {
		network = "tcp"
	}
	stream := map[string]any{
		"network":  network,
		"security": "tls",
	}
	tlsSettings := map[string]any{}
	if sni := q.Get("sni"); sni != "" {
		tlsSettings["serverName"] = sni
	} else {
		tlsSettings["serverName"] = host
	}
	if fp := q.Get("fp"); fp != "" {
		tlsSettings["fingerprint"] = fp
	}
	stream["tlsSettings"] = tlsSettings
	switch network {
	case "ws":
		ws := map[string]any{"path": q.Get("path")}
		if h := q.Get("host"); h != "" {
			ws["headers"] = map[string]any{"Host": h}
		}
		stream["wsSettings"] = ws
	case "grpc":
		stream["grpcSettings"] = map[string]any{"serviceName": q.Get("serviceName")}
	}
	outbound := map[string]any{
		"protocol": "trojan",
		"settings": map[string]any{
			"servers": []any{
				map[string]any{
					"address":  host,
					"port":     port,
					"password": password,
				},
			},
		},
		"streamSettings": stream,
	}
	return makeNode("trojan", name, host, port, outbound), nil
}

func parseShadowsocks(link string) (Node, error) {
	raw := strings.TrimPrefix(link, "ss://")
	name := ""
	if i := strings.Index(raw, "#"); i >= 0 {
		name, _ = url.QueryUnescape(raw[i+1:])
		raw = raw[:i]
	}
	var method, password, host string
	var port int

	if strings.Contains(raw, "@") {
		// ss://base64(method:pass)@host:port OR ss://method:pass@host:port
		parts := strings.SplitN(raw, "@", 2)
		userinfo := parts[0]
		if decoded, err := decodeBase64(userinfo); err == nil && strings.Contains(decoded, ":") {
			userinfo = decoded
		} else if u, err := url.QueryUnescape(userinfo); err == nil {
			userinfo = u
		}
		mp := strings.SplitN(userinfo, ":", 2)
		if len(mp) != 2 {
			return Node{}, fmt.Errorf("invalid ss userinfo")
		}
		method, password = mp[0], mp[1]
		hostPort := parts[1]
		if u, err := url.Parse("ss://" + raw); err == nil {
			host = u.Hostname()
			port, _ = strconv.Atoi(u.Port())
		} else {
			h, p, err := net.SplitHostPort(hostPort)
			if err != nil {
				return Node{}, err
			}
			host = h
			port, _ = strconv.Atoi(p)
		}
	} else {
		decoded, err := decodeBase64(raw)
		if err != nil {
			return Node{}, err
		}
		// method:password@host:port
		at := strings.LastIndex(decoded, "@")
		if at < 0 {
			return Node{}, fmt.Errorf("invalid ss link")
		}
		mp := strings.SplitN(decoded[:at], ":", 2)
		if len(mp) != 2 {
			return Node{}, fmt.Errorf("invalid ss method/pass")
		}
		method, password = mp[0], mp[1]
		h, p, err := net.SplitHostPort(decoded[at+1:])
		if err != nil {
			return Node{}, err
		}
		host = h
		port, _ = strconv.Atoi(p)
	}
	if name == "" {
		name = host
	}
	outbound := map[string]any{
		"protocol": "shadowsocks",
		"settings": map[string]any{
			"servers": []any{
				map[string]any{
					"address":  host,
					"port":     port,
					"method":   method,
					"password": password,
				},
			},
		},
	}
	return makeNode("shadowsocks", name, host, port, outbound), nil
}

func makeNode(proto, name, host string, port int, outbound map[string]any) Node {
	idSum := md5.Sum([]byte(fmt.Sprintf("%s|%s|%d|%v", proto, host, port, outbound)))
	id := hex.EncodeToString(idSum[:8])
	tag := "n-" + id
	outbound["tag"] = tag
	return Node{
		ID:       id,
		Name:     name,
		Protocol: proto,
		Address:  host,
		Port:     port,
		Outbound: outbound,
	}
}

func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatInt(int64(t), 10)
	case json.Number:
		return t.String()
	default:
		return fmt.Sprint(v)
	}
}

func strOr(v any, def string) string {
	s := str(v)
	if s == "" || s == "<nil>" {
		return def
	}
	return s
}

func strOrVal(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func pathOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func anyInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case string:
		n, _ := strconv.Atoi(t)
		return n
	case json.Number:
		n, _ := t.Int64()
		return int(n)
	default:
		return 0
	}
}
