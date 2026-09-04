package listen

import (
	"fmt"
	"net"

	"github.com/pires/go-proxyproto"
)

// asTCPConn unwraps proxyproto.Conn so SO_ORIGINAL_DST still works in direct mode.
func asTCPConn(conn net.Conn) (*net.TCPConn, error) {
	switch c := conn.(type) {
	case *net.TCPConn:
		return c, nil
	case *proxyproto.Conn:
		if tcp, ok := c.Raw().(*net.TCPConn); ok {
			return tcp, nil
		}
	}
	return nil, fmt.Errorf("not tcp")
}
