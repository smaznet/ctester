package listen

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pires/go-proxyproto"

	"github.com/aria/x-tester/internal/balancer"
	"github.com/aria/x-tester/internal/config"
	"github.com/aria/x-tester/internal/metrics"
	"github.com/aria/x-tester/internal/probe"
)

type Server struct {
	Cfg      config.Listen
	Balancer *balancer.Sticky
	Metrics  *metrics.Collector
	log      *log.Logger

	mu     sync.Mutex
	lners  []net.Listener
	cancel context.CancelFunc
}

func New(cfg config.Listen, bal *balancer.Sticky, logger *log.Logger, met *metrics.Collector) *Server {
	if logger == nil {
		logger = log.Default()
	}
	if met == nil {
		met = metrics.New()
	}
	return &Server{Cfg: cfg, Balancer: bal, Metrics: met, log: logger}
}

func (s *Server) Start(ctx context.Context) error {
	ctx, s.cancel = context.WithCancel(ctx)
	if s.Cfg.Unix != "" {
		ln, err := listenUnix(s.Cfg.Unix)
		if err != nil {
			return err
		}
		ln = s.maybeProxyProtocol(ln)
		s.mu.Lock()
		s.lners = append(s.lners, ln)
		s.mu.Unlock()
		go s.serve(ctx, ln, 0)
		return nil
	}
	for _, port := range s.Cfg.Port.Ports() {
		addr := net.JoinHostPort(s.Cfg.Host, strconv.Itoa(port))
		var ln net.Listener
		var err error
		if s.Cfg.Mode == "direct" {
			ln, err = listenTCPRedirect(addr)
		} else {
			ln, err = net.Listen("tcp", addr)
		}
		if err != nil {
			_ = s.Close()
			return fmt.Errorf("listen %s: %w", addr, err)
		}
		ln = s.maybeProxyProtocol(ln)
		p := port
		s.mu.Lock()
		s.lners = append(s.lners, ln)
		s.mu.Unlock()
		go s.serve(ctx, ln, p)
	}
	return nil
}

// maybeProxyProtocol wraps ln so RemoteAddr is the PROXY source (v1/v2).
// When enabled, peers must send a PROXY header (REQUIRE); otherwise the first
// read/write fails — matching Xray sockopt.acceptProxyProtocol.
func (s *Server) maybeProxyProtocol(ln net.Listener) net.Listener {
	if !s.Cfg.AcceptProxyProtocol {
		return ln
	}
	return &proxyproto.Listener{
		Listener:          ln,
		ReadHeaderTimeout: 5 * time.Second,
	}
}

func (s *Server) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var first error
	for _, ln := range s.lners {
		if err := ln.Close(); err != nil && first == nil {
			first = err
		}
	}
	s.lners = nil
	return first
}

func (s *Server) serve(ctx context.Context, ln net.Listener, inPort int) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				s.log.Printf("accept: %v", err)
				return
			}
		}
		go s.handle(ctx, conn, inPort)
	}
}

func (s *Server) handle(ctx context.Context, conn net.Conn, inPort int) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	switch s.Cfg.Mode {
	case "socks5":
		s.handleSocks5(conn, inPort)
	case "http":
		s.handleHTTP(conn, inPort)
	case "direct":
		s.handleDirect(conn, inPort)
	default:
		s.log.Printf("unknown mode %s", s.Cfg.Mode)
	}
}

func (s *Server) pick(username string, conn net.Conn, inPort int) (int, bool) {
	host, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	port, _, ok := s.Balancer.Pick(username, host, inPort)
	return port, ok
}

func (s *Server) handleSocks5(conn net.Conn, inPort int) {
	br := bufio.NewReader(conn)
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(br, hdr); err != nil || hdr[0] != 0x05 {
		return
	}
	methods := make([]byte, int(hdr[1]))
	if _, err := io.ReadFull(br, methods); err != nil {
		return
	}
	okMethod := false
	for _, m := range methods {
		if m == 0x02 {
			okMethod = true
			break
		}
	}
	if !okMethod {
		_, _ = conn.Write([]byte{0x05, 0xff})
		return
	}
	_, _ = conn.Write([]byte{0x05, 0x02})

	authVer := make([]byte, 2)
	if _, err := io.ReadFull(br, authVer); err != nil || authVer[0] != 0x01 {
		return
	}
	ubuf := make([]byte, int(authVer[1]))
	if _, err := io.ReadFull(br, ubuf); err != nil {
		return
	}
	plen, err := br.ReadByte()
	if err != nil {
		return
	}
	pbuf := make([]byte, int(plen))
	if _, err := io.ReadFull(br, pbuf); err != nil {
		return
	}
	username := string(ubuf)
	if string(pbuf) != s.Cfg.Password {
		_, _ = conn.Write([]byte{0x01, 0x01})
		return
	}
	_, _ = conn.Write([]byte{0x01, 0x00})

	req := make([]byte, 4)
	if _, err := io.ReadFull(br, req); err != nil || req[0] != 0x05 {
		return
	}
	if req[1] != 0x01 {
		writeSocksReply(conn, 0x07)
		return
	}
	target, err := readSocksAddr(br, req[3])
	if err != nil {
		writeSocksReply(conn, 0x01)
		return
	}
	localPort, ok := s.pick(username, conn, inPort)
	if !ok {
		writeSocksReply(conn, 0x01)
		return
	}
	remote, err := probe.DialViaSocks(localPort, "tcp", target)
	if err != nil {
		writeSocksReply(conn, 0x05)
		return
	}
	defer remote.Close()
	writeSocksReply(conn, 0x00)
	_ = conn.SetDeadline(time.Time{})
	s.relay(conn, br, remote)
}

func writeSocksReply(conn net.Conn, rep byte) {
	_, _ = conn.Write([]byte{0x05, rep, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
}

func readSocksAddr(br *bufio.Reader, atyp byte) (string, error) {
	var host string
	switch atyp {
	case 0x01:
		b := make([]byte, 4)
		if _, err := io.ReadFull(br, b); err != nil {
			return "", err
		}
		host = net.IP(b).String()
	case 0x03:
		l, err := br.ReadByte()
		if err != nil {
			return "", err
		}
		b := make([]byte, int(l))
		if _, err := io.ReadFull(br, b); err != nil {
			return "", err
		}
		host = string(b)
	case 0x04:
		b := make([]byte, 16)
		if _, err := io.ReadFull(br, b); err != nil {
			return "", err
		}
		host = net.IP(b).String()
	default:
		return "", fmt.Errorf("bad atyp")
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(br, pb); err != nil {
		return "", err
	}
	port := binary.BigEndian.Uint16(pb)
	return net.JoinHostPort(host, strconv.Itoa(int(port))), nil
}

func (s *Server) handleHTTP(conn net.Conn, inPort int) {
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	username, password, ok := parseBasicProxyAuth(req.Header.Get("Proxy-Authorization"))
	if !ok || password != s.Cfg.Password {
		_, _ = io.WriteString(conn, "HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"x-tester\"\r\n\r\n")
		return
	}
	localPort, ok := s.pick(username, conn, inPort)
	if !ok {
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}

	if req.Method == http.MethodConnect {
		host := req.Host
		if !strings.Contains(host, ":") {
			host += ":443"
		}
		remote, err := probe.DialViaSocks(localPort, "tcp", host)
		if err != nil {
			_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
			return
		}
		defer remote.Close()
		_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = conn.SetDeadline(time.Time{})
		s.relay(conn, br, remote)
		return
	}

	target := req.URL.Host
	if target == "" {
		_, _ = io.WriteString(conn, "HTTP/1.1 400 Bad Request\r\n\r\n")
		return
	}
	if !strings.Contains(target, ":") {
		target += ":80"
	}
	remote, err := probe.DialViaSocks(localPort, "tcp", target)
	if err != nil {
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer remote.Close()
	req.Header.Del("Proxy-Authorization")
	req.Header.Del("Proxy-Connection")
	_ = conn.SetDeadline(time.Time{})
	if err := req.Write(remote); err != nil {
		return
	}
	s.relay(conn, br, remote)
}

func parseBasicProxyAuth(h string) (user, pass string, ok bool) {
	const prefix = "Basic "
	if !strings.HasPrefix(h, prefix) {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(h[len(prefix):]))
	if err != nil {
		return "", "", false
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func (s *Server) handleDirect(conn net.Conn, inPort int) {
	// Force PROXY parse first so hash_ip sees header source (not Freedom/HAProxy hop).
	clientIP, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	br := bufio.NewReader(conn)

	listenPort := inPort
	if listenPort == 0 {
		listenPort = s.Cfg.Port.Start
	}
	target, err := resolveDirectTarget(conn, br, s.Cfg.Host, listenPort)
	if err != nil {
		s.log.Printf("direct dest: client=%s err=%v", clientIP, err)
		return
	}
	localPort, ok := s.pick("", conn, inPort)
	if !ok {
		s.log.Printf("direct pick: client=%s dest=%s no node", clientIP, target)
		return
	}
	s.log.Printf("direct: client=%s dest=%s → local:%d", clientIP, target, localPort)
	remote, err := probe.DialViaSocks(localPort, "tcp", target)
	if err != nil {
		s.log.Printf("direct dial: client=%s dest=%s err=%v", clientIP, target, err)
		return
	}
	defer remote.Close()
	_ = conn.SetDeadline(time.Time{})
	s.relay(conn, br, remote)
}

func (s *Server) relay(client net.Conn, br *bufio.Reader, remote net.Conn) {
	s.Metrics.ConnOpen()
	defer s.Metrics.ConnClose()

	up := &countWriter{w: remote, add: s.Metrics.AddUp}
	down := &countWriter{w: client, add: s.Metrics.AddDown}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if br != nil && br.Buffered() > 0 {
			pending := make([]byte, br.Buffered())
			_, _ = br.Read(pending)
			_, _ = up.Write(pending)
		}
		_, _ = io.Copy(up, client)
		closeWrite(remote)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(down, remote)
		closeWrite(client)
	}()
	wg.Wait()
}

type countWriter struct {
	w   io.Writer
	add func(int64)
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if n > 0 {
		c.add(int64(n))
	}
	return n, err
}

func closeWrite(c net.Conn) {
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
}
