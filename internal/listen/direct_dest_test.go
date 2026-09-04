package listen

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestIsLocalListenTarget(t *testing.T) {
	if !isLocalListenTarget("127.0.0.1:1080", "127.0.0.1", 1080) {
		t.Fatal("loopback listen should be local")
	}
	if isLocalListenTarget("1.2.3.4:80", "127.0.0.1", 1080) {
		t.Fatal("external should not be local")
	}
}

func TestSniffHTTPHost(t *testing.T) {
	raw := "GET / HTTP/1.1\r\nHost: ifconfig.io\r\nUser-Agent: curl\r\n\r\n"
	br := bufio.NewReader(strings.NewReader(raw))
	got, err := sniffHTTPHost(br)
	if err != nil {
		t.Fatal(err)
	}
	if got != "ifconfig.io:80" {
		t.Fatalf("got %q", got)
	}
	// buffered data still available for relay
	peek, _ := br.Peek(3)
	if !bytes.Equal(peek, []byte("GET")) {
		t.Fatalf("peek corrupted: %q", peek)
	}
}
