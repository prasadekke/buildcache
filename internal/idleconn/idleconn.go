// Package idleconn wraps a net.Conn so that any single read or write that
// makes no progress for a set duration fails with a timeout, instead of
// blocking forever.
//
// A plain whole-request deadline is the wrong shape for buildcache: a
// "put" of a large object over a slow link can legitimately take a long
// time, while a dead peer or a black-holed network can make even a tiny
// "get" hang indefinitely. What actually distinguishes the two is whether
// bytes are still moving, so the deadline here is re-armed before every
// Read and every write chunk. A stalled connection fails within the
// timeout; a slow but progressing one never does.
package idleconn

import (
	"net"
	"time"
)

// writeChunk bounds how much one underlying Write is asked to send under a
// single deadline, so a large write is progress-checked every chunk rather
// than given one deadline for the whole thing.
const writeChunk = 64 << 10

// Wrap returns conn with an inactivity timeout applied to every read and
// write. A timeout <= 0 returns conn unchanged.
func Wrap(conn net.Conn, timeout time.Duration) net.Conn {
	if timeout <= 0 {
		return conn
	}
	return &idleConn{Conn: conn, timeout: timeout}
}

type idleConn struct {
	net.Conn
	timeout time.Duration
}

func (c *idleConn) Read(p []byte) (int, error) {
	if err := c.Conn.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Read(p)
}

func (c *idleConn) Write(p []byte) (int, error) {
	var written int
	for len(p) > 0 {
		chunk := p
		if len(chunk) > writeChunk {
			chunk = chunk[:writeChunk]
		}
		if err := c.Conn.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
			return written, err
		}
		n, err := c.Conn.Write(chunk)
		written += n
		if err != nil {
			return written, err
		}
		p = p[n:]
	}
	return written, nil
}
