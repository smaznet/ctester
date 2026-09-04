//go:build !linux

package listen

import (
	"fmt"
	"net"
	"os"
	"strings"
)

func listenUnix(path string) (net.Listener, error) {
	if !strings.HasPrefix(path, "@") {
		_ = os.Remove(path)
	}
	return net.Listen("unix", path)
}

func listenTCPRedirect(addr string) (net.Listener, error) {
	// Transparent/redirect sockets need Linux. Fall back to plain listen for build/dev.
	return net.Listen("tcp", addr)
}

func originalDest(conn net.Conn) (string, error) {
	return "", fmt.Errorf("direct/dokodemo mode requires Linux (SO_ORIGINAL_DST)")
}
