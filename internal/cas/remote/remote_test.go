package remote_test

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prasadekke/buildcache/internal/cas"
	"github.com/prasadekke/buildcache/internal/cas/disk"
	"github.com/prasadekke/buildcache/internal/cas/remote"
	"github.com/prasadekke/buildcache/internal/protocol"
	"github.com/prasadekke/buildcache/internal/testpki"
	"github.com/prasadekke/buildcache/internal/tlsconfig"
)

// newRemoteStore wires a remote.Store to a real protocol.Server backed by a
// throwaway disk.Store, over an in-memory full-duplex pipe -- the same
// shape cacheprog uses against a real cacheserver, minus the Unix socket.
func newRemoteStore(t *testing.T) (*remote.Store, *disk.Store, <-chan error) {
	t.Helper()
	diskStore, err := disk.New(t.TempDir())
	if err != nil {
		t.Fatalf("disk.New: %v", err)
	}
	handler := cas.NewHandler(diskStore)

	serverConn, clientConn := net.Pipe()
	done := make(chan error, 1)
	go func() {
		srv := protocol.NewServer(serverConn, serverConn, handler)
		done <- srv.Run()
	}()

	remoteStore, err := remote.New(clientConn)
	if err != nil {
		t.Fatalf("remote.New: %v", err)
	}
	return remoteStore, diskStore, done
}

// outputIDOf returns the OutputID the go command would compute for body:
// its SHA-256. remote.Store checks inline bodies against it.
func outputIDOf(body []byte) []byte {
	sum := sha256.Sum256(body)
	return sum[:]
}

func waitServer(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("server Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("server did not shut down in time")
	}
}

func TestRemoteStore_PutGetRoundTrip(t *testing.T) {
	s, diskStore, done := newRemoteStore(t)
	defer diskStore.Close()

	actionID := []byte("action")
	outputID := []byte("output")
	body := []byte("hello from remote")

	diskPath, err := s.Put(actionID, outputID, bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if diskPath == "" {
		t.Fatal("Put returned an empty DiskPath")
	}

	entry, ok, err := s.Get(actionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("Get: ok = false, want true after Put")
	}
	if string(entry.OutputID) != string(outputID) {
		t.Errorf("entry.OutputID = %q, want %q", entry.OutputID, outputID)
	}
	if entry.Size != int64(len(body)) {
		t.Errorf("entry.Size = %d, want %d", entry.Size, len(body))
	}
	if entry.DiskPath == "" {
		t.Error("entry.DiskPath is empty")
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitServer(t, done)
}

func TestRemoteStore_GetMiss(t *testing.T) {
	s, diskStore, done := newRemoteStore(t)
	defer diskStore.Close()

	_, ok, err := s.Get([]byte("never-stored"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Fatal("Get: ok = true, want false for a never-stored action")
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitServer(t, done)
}

func TestRemoteStore_Close(t *testing.T) {
	s, diskStore, done := newRemoteStore(t)
	defer diskStore.Close()

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitServer(t, done)
}

// TestRemoteStore_InlineBodyMaterializesLocalCopy is the regression test
// for the cross-machine DiskPath bug: when the server wraps its handler
// with cas.InlineGetBodies (what cacheserver does for any connection where
// DiskPath might not be locally readable -- i.e. never a same-machine Unix
// socket, always a network one), remote.Store must not trust the server's
// DiskPath verbatim. It must materialize the inlined bytes to its own
// local file and report *that* path -- otherwise a genuinely remote
// cacheserver would hand back a path that doesn't exist on this machine,
// and the go command would fail trying to read it.
func TestRemoteStore_InlineBodyMaterializesLocalCopy(t *testing.T) {
	diskStore, err := disk.New(t.TempDir())
	if err != nil {
		t.Fatalf("disk.New: %v", err)
	}
	defer diskStore.Close()
	handler := cas.InlineGetBodies(cas.NewHandler(diskStore))

	serverConn, clientConn := net.Pipe()
	done := make(chan error, 1)
	go func() {
		srv := protocol.NewServer(serverConn, serverConn, handler)
		done <- srv.Run()
	}()

	s, err := remote.New(clientConn)
	if err != nil {
		t.Fatalf("remote.New: %v", err)
	}

	actionID := []byte("action")
	body := []byte("bytes that only exist on the 'other machine's' disk")
	serverDiskPath, err := diskStore.Put(actionID, outputIDOf(body), bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("seeding the server's store directly: %v", err)
	}

	entry, ok, err := s.Get(actionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("Get: ok = false, want true")
	}
	if entry.DiskPath == serverDiskPath {
		t.Fatalf("entry.DiskPath = %q, the server's own path -- remote.Store must materialize a local copy, not hand back a path that only exists on the server's machine", entry.DiskPath)
	}
	got, err := os.ReadFile(entry.DiskPath)
	if err != nil {
		t.Fatalf("reading materialized local copy at %s: %v", entry.DiskPath, err)
	}
	if string(got) != string(body) {
		t.Errorf("materialized copy = %q, want %q", got, body)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitServer(t, done)

	if _, err := os.Stat(entry.DiskPath); !os.IsNotExist(err) {
		t.Error("materialized temp file still exists after Close, want it cleaned up")
	}
}

func TestRemoteStore_InlineZeroByteBodyMaterializesLocalCopy(t *testing.T) {
	diskStore, err := disk.New(t.TempDir())
	if err != nil {
		t.Fatalf("disk.New: %v", err)
	}
	defer diskStore.Close()
	handler := cas.InlineGetBodies(cas.NewHandler(diskStore))

	serverConn, clientConn := net.Pipe()
	done := make(chan error, 1)
	go func() {
		srv := protocol.NewServer(serverConn, serverConn, handler)
		done <- srv.Run()
	}()

	s, err := remote.New(clientConn)
	if err != nil {
		t.Fatalf("remote.New: %v", err)
	}

	actionID := []byte("empty-action")
	serverDiskPath, err := diskStore.Put(actionID, outputIDOf(nil), bytes.NewReader(nil), 0)
	if err != nil {
		t.Fatalf("seeding the server's store directly: %v", err)
	}

	entry, ok, err := s.Get(actionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("Get: ok = false, want true")
	}
	if entry.DiskPath == serverDiskPath {
		t.Fatalf("entry.DiskPath = %q, the server's own path -- zero-byte inline hits must still materialize locally", entry.DiskPath)
	}
	got, err := os.ReadFile(entry.DiskPath)
	if err != nil {
		t.Fatalf("reading materialized local copy at %s: %v", entry.DiskPath, err)
	}
	if len(got) != 0 {
		t.Errorf("materialized zero-byte copy length = %d, want 0", len(got))
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitServer(t, done)
}

func TestDialTLS_ProtocolStartupTimeout(t *testing.T) {
	ca := testpki.NewCA(t)
	caFile := ca.File(t)
	serverCert, serverKey := ca.Issue(t, "server")
	clientCert, clientKey := ca.Issue(t, "client")

	serverTLS, err := tlsconfig.Server(serverCert, serverKey, caFile)
	if err != nil {
		t.Fatalf("tlsconfig.Server: %v", err)
	}
	clientTLS, err := tlsconfig.Client(clientCert, clientKey, caFile)
	if err != nil {
		t.Fatalf("tlsconfig.Client: %v", err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	defer ln.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if tlsConn, ok := conn.(*tls.Conn); ok {
			_ = tlsConn.Handshake()
		}
		time.Sleep(500 * time.Millisecond)
	}()

	start := time.Now()
	store, err := remote.DialTLS(ln.Addr().String(), clientTLS, 50*time.Millisecond)
	if err == nil {
		store.Close()
		t.Fatal("DialTLS err = nil, want timeout waiting for protocol startup")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("DialTLS took %v, want it bounded by the startup timeout", elapsed)
	}
	if !strings.Contains(err.Error(), "i/o timeout") {
		t.Fatalf("DialTLS err = %v, want an i/o timeout", err)
	}
	<-done
}

func TestNew_HandshakeFailure(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	serverConn.Close() // nothing will ever answer the handshake

	if _, err := remote.New(clientConn); err == nil {
		t.Fatal("New() err = nil, want an error when the handshake connection is already broken")
	}
}

// hitWithNoDiskPathHandler answers "get" with a non-miss response that
// still lacks a DiskPath -- a malformed but not impossible response from a
// misbehaving server -- and "put" the same way, to exercise remote.Store's
// own validation of what it gets back.
type hitWithNoDiskPathHandler struct{}

func (hitWithNoDiskPathHandler) Get(req *protocol.Request, resp *protocol.Response) error {
	resp.OutputID = []byte("out") // a hit (Miss stays false) but no DiskPath
	return nil
}

func (hitWithNoDiskPathHandler) Put(req *protocol.Request, resp *protocol.Response) error {
	return nil // "succeeds" but never sets DiskPath
}

func TestGet_HitWithoutDiskPathIsAnError(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	done := make(chan error, 1)
	go func() {
		srv := protocol.NewServer(serverConn, serverConn, hitWithNoDiskPathHandler{})
		done <- srv.Run()
	}()

	s, err := remote.New(clientConn)
	if err != nil {
		t.Fatalf("remote.New: %v", err)
	}
	if _, _, err := s.Get([]byte("a")); err == nil {
		t.Fatal("Get() err = nil, want an error for a hit response with no DiskPath")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitServer(t, done)
}

func TestGet_TransportErrorPropagates(t *testing.T) {
	s, diskStore, _ := newRemoteStore(t)
	defer diskStore.Close()

	// Close the connection out from under the store, then try to use it.
	s.Close()

	if _, _, err := s.Get([]byte("a")); err == nil {
		t.Fatal("Get() err = nil, want an error once the underlying connection is closed")
	}
}

func TestPut_TransportErrorPropagates(t *testing.T) {
	s, diskStore, _ := newRemoteStore(t)
	defer diskStore.Close()

	s.Close()

	if _, err := s.Put([]byte("a"), []byte("b"), bytes.NewReader([]byte("x")), 1); err == nil {
		t.Fatal("Put() err = nil, want an error once the underlying connection is closed")
	}
}

func TestPut_NoDiskPathIsAnError(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	done := make(chan error, 1)
	go func() {
		srv := protocol.NewServer(serverConn, serverConn, hitWithNoDiskPathHandler{})
		done <- srv.Run()
	}()

	s, err := remote.New(clientConn)
	if err != nil {
		t.Fatalf("remote.New: %v", err)
	}
	if _, err := s.Put([]byte("a"), []byte("b"), bytes.NewReader([]byte("x")), 1); err == nil {
		t.Fatal("Put() err = nil, want an error for a response with no DiskPath")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitServer(t, done)
}

// TestRemoteStore_StalledServerTimesOut is the regression test for remote
// calls having no deadline once the handshake was done: a server that stops
// answering mid-session must fail the call within the inactivity timeout,
// flagged as cas.ErrUnavailable so tiered stops consulting it -- and every
// later call must fail immediately rather than waiting out the timeout
// again.
func TestRemoteStore_StalledServerTimesOut(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		serverConn.Write([]byte(`{"ID":0,"KnownCommands":["get","put","close"],"Version":1}` + "\n"))
		io.Copy(io.Discard, serverConn) // read requests forever, never answer
	}()

	s, err := remote.NewConn(clientConn, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("NewConn: %v", err)
	}
	defer s.Close()

	start := time.Now()
	_, _, err = s.Get([]byte("a"))
	if !errors.Is(err, cas.ErrUnavailable) {
		t.Fatalf("Get err = %v, want cas.ErrUnavailable", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Get took %v, want it bounded by the inactivity timeout", elapsed)
	}

	start = time.Now()
	if _, err := s.Put([]byte("a"), []byte("b"), bytes.NewReader([]byte("x")), 1); !errors.Is(err, cas.ErrUnavailable) {
		t.Fatalf("Put after a timeout: err = %v, want cas.ErrUnavailable", err)
	}
	if elapsed := time.Since(start); elapsed > 25*time.Millisecond {
		t.Errorf("Put after a timeout took %v, want it to fail fast", elapsed)
	}
}

func TestRemoteStore_InlineBodyIntegrityMismatch(t *testing.T) {
	diskStore, err := disk.New(t.TempDir())
	if err != nil {
		t.Fatalf("disk.New: %v", err)
	}
	defer diskStore.Close()

	serverConn, clientConn := net.Pipe()
	done := make(chan error, 1)
	go func() {
		srv := protocol.NewServer(serverConn, serverConn, cas.InlineGetBodies(cas.NewHandler(diskStore)))
		done <- srv.Run()
	}()
	s, err := remote.New(clientConn)
	if err != nil {
		t.Fatalf("remote.New: %v", err)
	}

	body := []byte("these bytes don't hash to the claimed OutputID")
	if _, err := diskStore.Put([]byte("a"), outputIDOf([]byte("something else")), bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	_, ok, err := s.Get([]byte("a"))
	if err == nil || ok {
		t.Fatalf("Get = ok %v err %v, want an integrity error", ok, err)
	}
	if errors.Is(err, cas.ErrUnavailable) {
		t.Errorf("Get err = %v, want a per-request error, not cas.ErrUnavailable", err)
	}

	s.Close()
	waitServer(t, done)
}

func TestRemoteStore_FetchHandsOverInlineBody(t *testing.T) {
	diskStore, err := disk.New(t.TempDir())
	if err != nil {
		t.Fatalf("disk.New: %v", err)
	}
	defer diskStore.Close()

	serverConn, clientConn := net.Pipe()
	done := make(chan error, 1)
	go func() {
		srv := protocol.NewServer(serverConn, serverConn, cas.InlineGetBodies(cas.NewHandler(diskStore)))
		done <- srv.Run()
	}()
	s, err := remote.New(clientConn)
	if err != nil {
		t.Fatalf("remote.New: %v", err)
	}

	body := []byte("fetched without a temp file")
	if _, err := diskStore.Put([]byte("a"), outputIDOf(body), bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	entry, r, ok, err := s.Fetch([]byte("a"))
	if err != nil || !ok || r == nil {
		t.Fatalf("Fetch = ok %v body %v err %v, want a hit with an inline body", ok, r, err)
	}
	if entry.DiskPath != "" {
		t.Errorf("entry.DiskPath = %q, want empty (no temp file materialized)", entry.DiskPath)
	}
	got, _ := io.ReadAll(r)
	if !bytes.Equal(got, body) || entry.Size != int64(len(body)) {
		t.Errorf("Fetch body = %q (size %d), want %q", got, entry.Size, body)
	}

	s.Close()
	waitServer(t, done)
}

func TestRemoteStore_ReadOnlyServerPut(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	done := make(chan error, 1)
	go func() {
		srv := protocol.NewServer(serverConn, serverConn, hitWithNoDiskPathHandler{})
		srv.ReadOnly = true
		done <- srv.Run()
	}()
	s, err := remote.New(clientConn)
	if err != nil {
		t.Fatalf("remote.New: %v", err)
	}
	if _, err := s.Put([]byte("a"), []byte("b"), bytes.NewReader([]byte("x")), 1); !errors.Is(err, cas.ErrReadOnly) {
		t.Fatalf("Put err = %v, want cas.ErrReadOnly", err)
	}
	s.Close()
	waitServer(t, done)
}
