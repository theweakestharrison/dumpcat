// Package tunnel has the pieces shared by the ways DumpCat reaches a database
// it cannot connect to directly (agents, WireGuard): copying a connection both
// ways, and telling a MySQL client why a tunnel could not be opened.
package tunnel

import (
	"io"
	"net"
	"time"
)

// Pipe copies both ways until either side ends, then closes both.
func Pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	a.Close()
	b.Close()
	<-done
}

// MySQLError answers a client waiting for the server greeting with an error
// packet (tunnels only carry the MySQL protocol), so mysql, mysqldump and the
// Go driver show why the tunnel failed instead of "lost connection".
func MySQLError(c net.Conn, msg string) {
	if len(msg) > 500 {
		msg = msg[:500]
	}
	// 1105 ER_UNKNOWN_ERROR; no SQL state: no capabilities are agreed yet
	payload := append([]byte{0xff, 0x51, 0x04}, msg...)
	pkt := append([]byte{byte(len(payload)), byte(len(payload) >> 8), byte(len(payload) >> 16), 0}, payload...)
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _ = c.Write(pkt)
}
