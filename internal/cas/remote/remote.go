// Package remote implements a cas.Store that talks to a remote cacheserver
// over the wire protocol (internal/protocol), rather than storing objects
// itself. It's the client-side counterpart to cas.Handler: where Handler
// adapts a Store to the protocol for a server to speak, remote.Store adapts
// the protocol back into a Store for a client (cacheprog, via
// internal/cas/tiered) to call like any other backend.
package remote

import (
	"fmt"
	"io"

	"github.com/prasadekke/buildcache/internal/cas"
	"github.com/prasadekke/buildcache/internal/protocol"
)

// Store adapts a protocol.Client to cas.Store.
//
// DiskPath assumption: the cas.Entry.DiskPath this Store returns from Get
// is whatever the remote server reported, which must be readable as a
// local file by this process. That holds today because cacheserver only
// ever listens on a Unix domain socket (see internal/sockpath) -- same
// machine, same filesystem. A future network-facing cacheserver would need
// this package to actually fetch body bytes over the wire instead of
// trusting a bare path from the other end.
type Store struct {
	client *protocol.Client
}

// New performs the protocol handshake over conn and returns a ready-to-use
// Store. New takes ownership of conn and closes it if the handshake fails.
func New(conn io.ReadWriteCloser) (*Store, error) {
	client, err := protocol.NewClient(conn)
	if err != nil {
		return nil, err
	}
	return &Store{client: client}, nil
}

// Get implements cas.Store.
func (s *Store) Get(actionID []byte) (cas.Entry, bool, error) {
	resp, err := s.client.Get(actionID)
	if err != nil {
		return cas.Entry{}, false, err
	}
	if resp.Miss {
		return cas.Entry{}, false, nil
	}
	if resp.DiskPath == "" {
		return cas.Entry{}, false, fmt.Errorf("remote get for action %x: server reported a hit with no DiskPath", actionID)
	}

	entry := cas.Entry{
		OutputID: resp.OutputID,
		Size:     resp.Size,
		DiskPath: resp.DiskPath,
	}
	if resp.Time != nil {
		entry.Time = *resp.Time
	}
	return entry, true, nil
}

// Put implements cas.Store.
func (s *Store) Put(actionID, outputID []byte, body io.Reader, size int64) (string, error) {
	resp, err := s.client.Put(actionID, outputID, body, size)
	if err != nil {
		return "", err
	}
	if resp.DiskPath == "" {
		return "", fmt.Errorf("remote put for action %x: server didn't return a DiskPath", actionID)
	}
	return resp.DiskPath, nil
}

// Close implements cas.Store.
func (s *Store) Close() error {
	return s.client.Close()
}
