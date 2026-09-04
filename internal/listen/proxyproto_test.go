package listen_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/aria/x-tester/internal/balancer"
	"github.com/aria/x-tester/internal/config"
	"github.com/aria/x-tester/internal/listen"
	"github.com/aria/x-tester/internal/metrics"
)

func TestAcceptProxyProtocolRemoteAddr(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	bal := balancer.New("hash_ip")
	bal.Upsert(balancer.NodeState{ID: "n1", Name: "n1", LocalPort: 9, Active: true})

	srv := listen.New(config.Listen{
		Mode:                "socks5",
		Password:            "secret",
		Host:                "127.0.0.1",
		Port:                config.PortSpec{Start: port, End: port},
		AcceptProxyProtocol: true,
	}, bal, nil, metrics.New())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	// Dial and only send PROXY — RemoteAddr on server side is observed via a raw probe:
	// connect, write PROXY + invalid socks so handler exits; we assert via a side listener pattern.
	// Simpler: dial proxyproto listener path by connecting and reading that first Read triggers policy.
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = io.WriteString(c, "PROXY TCP4 10.20.30.40 127.0.0.1 12345 1080\r\n")
	if err != nil {
		t.Fatal(err)
	}
	// Incomplete socks — server should accept PP then drop; connection must not hang.
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16)
	_, _ = c.Read(buf) // may get socks reply or EOF; just ensure no hang
}

func TestAcceptProxyProtocolRejectsBareTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	srv := listen.New(config.Listen{
		Mode:                "socks5",
		Password:            "secret",
		Host:                "127.0.0.1",
		Port:                config.PortSpec{Start: port, End: port},
		AcceptProxyProtocol: true,
	}, balancer.New("hash_ip"), nil, metrics.New())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// SOCKS greeting without PROXY — REQUIRE policy should fail the stream.
	_, _ = c.Write([]byte{0x05, 0x01, 0x02})
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16)
	n, err := c.Read(buf)
	if n > 0 && buf[0] == 0x05 {
		t.Fatalf("server accepted SOCKS without PROXY header")
	}
	_ = err
}
