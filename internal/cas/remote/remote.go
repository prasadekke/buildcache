// Package remote implements a cas.Store that talks to a remote cacheserver
// over the wire protocol (internal/protocol), rather than storing objects
// itself. It's the client-side counterpart to cas.Handler: where Handler
// adapts a Store to the protocol for a server to speak, remote.Store adapts
// the protocol back into a Store for a client (cacheprog, via
// internal/cas/tiered) to call like any other backend.
package remote

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/prasadekke/buildcache/internal/cas"
	"github.com/prasadekke/buildcache/internal/idleconn"
	"github.com/prasadekke/buildcache/internal/protocol"
)

// Store adapts a protocol.Client to cas.Store.
//
// A "get" hit's DiskPath is only trustworthy when client and server share
// a filesystem, which is guaranteed for a same-machine Unix socket but not
// for a network connection. When the server instead inlines the body
// (Response.Body/BodySize -- see cacheserver's use of
// internal/cas.InlineGetBodies), Store checks the bytes against their
// OutputID and either hands them over directly (Fetch, which
// internal/cas/tiered uses to write them straight into its local tier) or
// materializes them to a local temp file and reports that path instead of
// trusting DiskPath verbatim (Get). Those temp files are cleaned up on
// Close.
//
// Errors that mean the connection itself is gone (see protocol.Client)
// wrap cas.ErrUnavailable; a Put to a read-only server wraps
// cas.ErrReadOnly.
type Store struct {
	client *protocol.Client

	mu       sync.Mutex
	tmpFiles []string
}

// New performs the protocol handshake over conn and returns a ready-to-use
// Store. New takes ownership of conn and closes it if the handshake fails.
// It applies no timeouts of its own; see NewConn for that.
func New(conn io.ReadWriteCloser) (*Store, error) {
	client, err := protocol.NewClient(conn)
	if err != nil {
		return nil, err
	}
	return &Store{client: client}, nil
}

// NewConn is New for a network connection, with an inactivity timeout
// (see internal/idleconn) applied to every read and write -- the startup
// handshake included. A server that stops responding mid-session then
// fails the call in progress within timeout, rather than hanging the build
// that's waiting on it.
func NewConn(conn net.Conn, timeout time.Duration) (*Store, error) {
	return New(idleconn.Wrap(conn, timeout))
}

// DialTLS dials addr over TCP+TLS (typically mTLS -- see
// internal/tlsconfig) and returns a ready-to-use Store. timeout bounds the
// dial and TLS handshake, then serves as NewConn's inactivity timeout for
// the rest of the session.
func DialTLS(addr string, tlsConfig *tls.Config, timeout time.Duration) (*Store, error) {
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := tls.DialWithDialer(dialer, "tcp", addr, tlsConfig)
	if err != nil {
		return nil, fmt.Errorf("dialing %s: %w", addr, err)
	}
	store, err := NewConn(conn, timeout)
	if err != nil {
		return nil, fmt.Errorf("protocol startup with %s: %w", addr, err)
	}
	return store, nil
}

// StoreDir returns the cache directory the server advertised at startup,
// or "" if it didn't advertise one.
func (s *Store) StoreDir() string { return s.client.StoreDir() }

// Fetch implements cas.Fetcher.
func (s *Store) Fetch(actionID []byte) (cas.Entry, io.Reader, bool, error) {
	resp, err := s.client.Get(actionID)
	if err != nil {
		return cas.Entry{}, nil, false, s.classify(err)
	}
	if resp.Miss {
		return cas.Entry{}, nil, false, nil
	}

	entry := cas.Entry{
		OutputID: resp.OutputID,
		Size:     resp.Size,
	}
	if resp.Time != nil {
		entry.Time = *resp.Time
	}

	switch {
	case resp.BodyInline && resp.Body != nil:
		// protocol.Client always decodes an inline body into memory, so
		// this is a bytes.Reader in practice.
		rs, ok := resp.Body.(io.ReadSeeker)
		if !ok {
			return cas.Entry{}, nil, false, fmt.Errorf("remote get for action %x: inline body is not seekable", actionID)
		}
		if err := cas.CheckOutputID(resp.OutputID, rs); err != nil {
			return cas.Entry{}, nil, false, fmt.Errorf("remote get for action %x: %w", actionID, err)
		}
		entry.Size = resp.BodySize
		return entry, rs, true, nil
	case resp.DiskPath != "":
		// No inline body -- trust DiskPath as-is. Correct for a
		// same-machine Unix-socket server; see the package doc comment.
		entry.DiskPath = resp.DiskPath
		return entry, nil, true, nil
	default:
		return cas.Entry{}, nil, false, fmt.Errorf("remote get for action %x: server reported a hit with neither DiskPath nor an inline body", actionID)
	}
}

// Get implements cas.Store.
func (s *Store) Get(actionID []byte) (cas.Entry, bool, error) {
	entry, body, ok, err := s.Fetch(actionID)
	if err != nil || !ok || body == nil {
		return entry, ok, err
	}
	path, err := s.materialize(body)
	if err != nil {
		return cas.Entry{}, false, fmt.Errorf("materializing remote hit for action %x: %w", actionID, err)
	}
	entry.DiskPath = path
	return entry, true, nil
}

// materialize writes body to a new temp file and records it for cleanup
// on Close, returning its path.
func (s *Store) materialize(body io.Reader) (string, error) {
	f, err := os.CreateTemp("", "buildcache-remote-*")
	if err != nil {
		return "", err
	}
	defer f.Close()

	if _, err := io.Copy(f, body); err != nil {
		os.Remove(f.Name())
		return "", err
	}

	s.mu.Lock()
	s.tmpFiles = append(s.tmpFiles, f.Name())
	s.mu.Unlock()
	return f.Name(), nil
}

// Put implements cas.Store.
func (s *Store) Put(actionID, outputID []byte, body io.Reader, size int64) (string, error) {
	if !s.client.CanPut() {
		return "", fmt.Errorf("remote put for action %x: %w", actionID, cas.ErrReadOnly)
	}
	resp, err := s.client.Put(actionID, outputID, body, size)
	if err != nil {
		return "", s.classify(err)
	}
	if resp.DiskPath == "" {
		return "", fmt.Errorf("remote put for action %x: server didn't return a DiskPath", actionID)
	}
	return resp.DiskPath, nil
}

// classify marks err as cas.ErrUnavailable if it left the connection
// unusable, as opposed to failing just that one request (an error the
// server reported, or a body rejected before anything was sent).
func (s *Store) classify(err error) error {
	if s.client.Err() == nil {
		return err
	}
	return fmt.Errorf("%w: %w", cas.ErrUnavailable, err)
}

// Close implements cas.Store: it removes any temp files materialized by
// Get, then closes the underlying connection.
func (s *Store) Close() error {
	s.mu.Lock()
	tmpFiles := s.tmpFiles
	s.tmpFiles = nil
	s.mu.Unlock()
	for _, p := range tmpFiles {
		os.Remove(p)
	}
	return s.client.Close()
}
