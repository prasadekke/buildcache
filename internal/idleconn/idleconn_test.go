package idleconn

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"
)

func TestWrap_StalledReadTimesOut(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	c := Wrap(a, 50*time.Millisecond)
	start := time.Now()
	_, err := c.Read(make([]byte, 1))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Read err = %v, want a deadline error", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Read took %v, want it bounded by the idle timeout", elapsed)
	}
}

func TestWrap_ProgressingWriteOutlivesTimeout(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	const timeout = 50 * time.Millisecond
	c := Wrap(a, timeout)

	// The reader drains slowly enough that the whole write takes several
	// timeouts' worth of wall time, but never stalls for a full one.
	payload := make([]byte, 6*writeChunk)
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, writeChunk)
		for read := 0; read < len(payload); {
			time.Sleep(timeout / 3)
			n, err := b.Read(buf)
			if err != nil {
				return
			}
			read += n
		}
	}()

	n, err := c.Write(payload)
	if err != nil {
		t.Fatalf("Write err = %v after %d bytes, want a slow-but-progressing write to succeed", err, n)
	}
	<-done
}

func TestWrap_ZeroTimeoutIsPassthrough(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if Wrap(a, 0) != a {
		t.Error("Wrap(conn, 0) should return conn unchanged")
	}
}
