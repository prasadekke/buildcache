package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prasadekke/buildcache/internal/cas"
	"github.com/prasadekke/buildcache/internal/metrics"
	"github.com/prasadekke/buildcache/internal/protocol"
)

func TestPrintStats(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w

	printStats(metrics.Snapshot{Gets: 10, Hits: 8, Puts: 3, PutBytes: 4096})

	w.Close()
	os.Stderr = orig

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("reading captured stderr: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"10 gets", "8 hits", "80.0% hit rate", "3 puts", "4096 bytes stored"} {
		if !strings.Contains(got, want) {
			t.Errorf("printStats output missing %q; got:\n%s", want, got)
		}
	}
}

// serveDaemon runs a minimal stand-in for cacheserver's Unix-socket
// listener, advertising storeDir, and returns its socket path.
func serveDaemon(t *testing.T, storeDir string) string {
	t.Helper()
	sockDir, err := os.MkdirTemp("", "cp") // short: sun_path is ~104 bytes on macOS
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				srv := protocol.NewServer(conn, conn, cas.NewHandler(nil))
				srv.StoreDir = storeDir
				srv.Run()
			}()
		}
	}()
	return sock
}

func TestDialLocalDaemon_SkipsDaemonServingSameDir(t *testing.T) {
	dir := t.TempDir()
	sock := serveDaemon(t, dir)
	logf := func(format string, args ...any) { t.Logf(format, args...) }

	if s := dialLocalDaemon(sock, dir, time.Second, logf); s != nil {
		s.Close()
		t.Fatal("dialLocalDaemon returned a remote tier for a daemon serving -dir itself, want nil")
	}
	// The same directory spelled differently is still the same directory.
	if s := dialLocalDaemon(sock, dir+string(filepath.Separator)+".", time.Second, logf); s != nil {
		s.Close()
		t.Fatal("dialLocalDaemon didn't recognize an equivalent spelling of the same directory")
	}
	if s := dialLocalDaemon(sock, t.TempDir(), time.Second, logf); s == nil {
		t.Fatal("dialLocalDaemon returned nil for a daemon serving a different directory, want a remote tier")
	} else {
		s.Close()
	}
}

func TestDialLocalDaemon_RefusesNonSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-socket")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	var logged bool
	if s := dialLocalDaemon(path, t.TempDir(), time.Second, func(string, ...any) { logged = true }); s != nil {
		s.Close()
		t.Fatal("dialLocalDaemon connected to something that isn't a socket")
	}
	if !logged {
		t.Error("expected a log line explaining why the socket was refused")
	}
}

func TestDialLocalDaemon_NoDaemonIsSilent(t *testing.T) {
	var logged bool
	if s := dialLocalDaemon(filepath.Join(t.TempDir(), "absent.sock"), t.TempDir(), time.Second, func(string, ...any) { logged = true }); s != nil {
		t.Fatal("dialLocalDaemon returned a store with no daemon running")
	}
	if logged {
		t.Error("no daemon running is the normal case and shouldn't be logged")
	}
}
