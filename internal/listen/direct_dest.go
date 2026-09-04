package listen

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/pires/go-proxyproto"
)

// resolveDirectTarget picks the dial target for mode=direct.
// Order: SO_ORIGINAL_DST → PROXY destination → HTTP Host / TLS SNI sniff
// (needed when Freedom redirect+proxyProtocol dials us and puts 127.0.0.1:listen
// in the PROXY dest field, wiping the real destination).
func resolveDirectTarget(conn net.Conn, br *bufio.Reader, listenHost string, listenPort int) (string, error) {
	if t, err := originalDest(conn); err == nil && t != "" && !isLocalListenTarget(t, listenHost, listenPort) {
		return t, nil
	}
	if t, ok := proxyDestination(conn); ok && !isLocalListenTarget(t, listenHost, listenPort) {
		return t, nil
	}
	if t, err := sniffDestination(br); err == nil && t != "" {
		return t, nil
	} else if err != nil {
		return "", fmt.Errorf("sniff dest: %w", err)
	}
	return "", fmt.Errorf("no destination (need SO_ORIGINAL_DST, PROXY dest, or HTTP/TLS sniff)")
}

func proxyDestination(conn net.Conn) (string, bool) {
	pc, ok := conn.(*proxyproto.Conn)
	if !ok {
		return "", false
	}
	// Ensure header is parsed.
	_ = conn.RemoteAddr()
	h := pc.ProxyHeader()
	if h == nil || h.DestinationAddr == nil {
		return "", false
	}
	return h.DestinationAddr.String(), true
}

func isLocalListenTarget(target, listenHost string, listenPort int) bool {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port != listenPort {
		return false
	}
	if host == listenHost || host == "0.0.0.0" || host == "::" || host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func sniffDestination(br *bufio.Reader) (string, error) {
	peek, err := br.Peek(1)
	if err != nil {
		return "", err
	}
	if peek[0] == 0x16 {
		return sniffTLSSNI(br)
	}
	return sniffHTTPHost(br)
}

func sniffHTTPHost(br *bufio.Reader) (string, error) {
	peek, err := br.Peek(8192)
	if err != nil && len(peek) == 0 {
		return "", err
	}
	end := bytes.Index(peek, []byte("\r\n\r\n"))
	if end < 0 {
		end = len(peek)
	}
	headers := string(peek[:end])
	for _, line := range strings.Split(headers, "\r\n") {
		if len(line) < 6 {
			continue
		}
		if strings.EqualFold(line[:5], "host:") {
			host := strings.TrimSpace(line[5:])
			if host == "" {
				return "", fmt.Errorf("empty Host")
			}
			if !strings.Contains(host, ":") {
				host = net.JoinHostPort(host, "80")
			}
			return host, nil
		}
	}
	return "", fmt.Errorf("no Host header")
}

func sniffTLSSNI(br *bufio.Reader) (string, error) {
	peek, err := br.Peek(1024)
	if err != nil && len(peek) < 5 {
		return "", fmt.Errorf("tls peek: %w", err)
	}
	sni, err := parseClientHelloSNI(peek)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(sni, "443"), nil
}

// parseClientHelloSNI extracts SNI from a TLS ClientHello record (best-effort).
func parseClientHelloSNI(data []byte) (string, error) {
	if len(data) < 5 || data[0] != 0x16 {
		return "", fmt.Errorf("not tls handshake")
	}
	recLen := int(data[3])<<8 | int(data[4])
	if len(data) < 5+recLen {
		// use what we have
		recLen = len(data) - 5
	}
	p := data[5 : 5+recLen]
	if len(p) < 4 || p[0] != 0x01 { // ClientHello
		return "", fmt.Errorf("not clienthello")
	}
	hsLen := int(p[1])<<16 | int(p[2])<<8 | int(p[3])
	if len(p) < 4+hsLen {
		hsLen = len(p) - 4
	}
	p = p[4 : 4+hsLen]
	if len(p) < 34 {
		return "", fmt.Errorf("clienthello too short")
	}
	p = p[34:] // skip version + random
	if len(p) < 1 {
		return "", fmt.Errorf("session id")
	}
	sidLen := int(p[0])
	p = p[1:]
	if len(p) < sidLen+2 {
		return "", fmt.Errorf("cipher suites")
	}
	p = p[sidLen:]
	csLen := int(p[0])<<8 | int(p[1])
	p = p[2:]
	if len(p) < csLen+1 {
		return "", fmt.Errorf("compress")
	}
	p = p[csLen:]
	compLen := int(p[0])
	p = p[1:]
	if len(p) < compLen+2 {
		return "", fmt.Errorf("extensions")
	}
	p = p[compLen:]
	extLen := int(p[0])<<8 | int(p[1])
	p = p[2:]
	if len(p) < extLen {
		extLen = len(p)
	}
	p = p[:extLen]
	for len(p) >= 4 {
		typ := int(p[0])<<8 | int(p[1])
		l := int(p[2])<<8 | int(p[3])
		p = p[4:]
		if len(p) < l {
			break
		}
		body := p[:l]
		p = p[l:]
		if typ != 0 { // server_name
			continue
		}
		if len(body) < 2 {
			break
		}
		list := body[2:]
		for len(list) >= 3 {
			nameType := list[0]
			nl := int(list[1])<<8 | int(list[2])
			list = list[3:]
			if len(list) < nl {
				break
			}
			if nameType == 0 {
				return string(list[:nl]), nil
			}
			list = list[nl:]
		}
	}
	return "", fmt.Errorf("no sni")
}
