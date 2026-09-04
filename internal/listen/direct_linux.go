//go:build linux

package listen

import (
	"context"
	"encoding/binary"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

func listenUnix(path string) (net.Listener, error) {
	if !strings.HasPrefix(path, "@") {
		_ = os.Remove(path)
	}
	return net.Listen("unix", path)
}

func listenTCPRedirect(addr string) (net.Listener, error) {
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var sockErr error
			if err := c.Control(func(fd uintptr) {
				sockErr = syscall.SetsockoptInt(int(fd), syscall.SOL_IP, syscall.IP_TRANSPARENT, 1)
			}); err != nil {
				return err
			}
			return sockErr
		},
	}
	return lc.Listen(context.Background(), "tcp", addr)
}

func originalDest(conn net.Conn) (string, error) {
	tcp, err := asTCPConn(conn)
	if err != nil {
		return "", err
	}
	rc, err := tcp.SyscallConn()
	if err != nil {
		return "", err
	}
	var (
		ip    net.IP
		port  int
		opErr error
	)
	err = rc.Control(func(fd uintptr) {
		const SO_ORIGINAL_DST = 80
		var rsa syscall.RawSockaddrInet4
		sz := uint32(unsafe.Sizeof(rsa))
		_, _, errno := syscall.Syscall6(
			syscall.SYS_GETSOCKOPT,
			fd,
			uintptr(syscall.SOL_IP),
			uintptr(SO_ORIGINAL_DST),
			uintptr(unsafe.Pointer(&rsa)),
			uintptr(unsafe.Pointer(&sz)),
			0,
		)
		if errno != 0 {
			opErr = errno
			return
		}
		ip = net.IPv4(rsa.Addr[0], rsa.Addr[1], rsa.Addr[2], rsa.Addr[3])
		port = int(binary.BigEndian.Uint16((*[2]byte)(unsafe.Pointer(&rsa.Port))[:]))
	})
	if err != nil {
		return "", err
	}
	if opErr != nil {
		return "", opErr
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(port)), nil
}
