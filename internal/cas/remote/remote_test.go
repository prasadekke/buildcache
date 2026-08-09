package remote_test

import (
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/prasadekke/buildcache/internal/cas"
	"github.com/prasadekke/buildcache/internal/cas/disk"
	"github.com/prasadekke/buildcache/internal/cas/remote"
	"github.com/prasadekke/buildcache/internal/protocol"
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
