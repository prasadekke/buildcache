// Package tiered composes two cas.Store backends into one: a fast Local
// tier (typically disk-backed, see internal/cas/disk) checked first, and a
// Remote tier (typically internal/cas/remote, talking to a shared
// cacheserver) consulted only on a local miss, with the result written
// through to Local so the next Get for the same action is served without
// another round trip.
//
// The point is combining GOCACHE's zero-IPC-overhead local speed with a
// shared cache's cross-machine benefit: a machine that's never built a
// given action pays the Remote round trip once, then behaves exactly like
// a machine with a warm local cache for every subsequent Get of that same
// action. Plain GOCACHEPROG usage without this tiering pays the JSON/IPC
// overhead of the wire protocol on *every* Get, even for actions this same
// machine already fetched a moment ago -- that overhead is what keeps a
// non-tiered GOCACHEPROG setup from ever quite matching GOCACHE's
// in-process speed on repeat local hits.
package tiered

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/prasadekke/buildcache/internal/cas"
)

// uploadQueueSize bounds how many finished Puts can be waiting to be
// pushed to Remote. Once it's full, Put blocks until the upload worker
// catches up -- a remote tier that can't keep up with the build slows it
// down to the remote's pace rather than silently dropping objects other
// machines would otherwise get.
const uploadQueueSize = 256

// Store composes Local and Remote into one cas.Store. Both must be
// non-nil; if there's no remote tier to fall back to, callers should just
// use Local directly instead of wrapping it here.
//
// A Store is safe for concurrent use as long as Local and Remote are.
type Store struct {
	Local  cas.Store
	Remote cas.Store

	// Logf receives a one-line message whenever Remote fails in a way
	// that's deliberately non-fatal (a flaky or unreachable shared cache
	// degrades to local-only behavior; it never fails the build). Defaults
	// to a no-op if nil.
	Logf func(format string, args ...any)

	// remoteDown is set once Remote reports cas.ErrUnavailable: every
	// later call would just fail the same way, so Remote is skipped for
	// the rest of this Store's life. pushDown is the same for uploads
	// alone, once Remote reports cas.ErrReadOnly.
	remoteDown atomic.Bool
	pushDown   atomic.Bool

	startOnce sync.Once
	uploads   chan upload
	pending   sync.WaitGroup
	workerWG  sync.WaitGroup
	closeMu   sync.RWMutex
	closed    bool
}

type upload struct {
	actionID, outputID []byte
	localPath          string
	size               int64
}

func (s *Store) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// remoteFailed records err from Remote, switching Remote (or just pushes
// to it) off for good if err says it's gone, and logging either way.
func (s *Store) remoteFailed(what string, actionID []byte, err error) {
	switch {
	case errors.Is(err, cas.ErrUnavailable):
		if s.remoteDown.CompareAndSwap(false, true) {
			s.logf("tiered: remote tier unavailable (%s for action %x), continuing with local disk only: %v", what, actionID, err)
		}
	case errors.Is(err, cas.ErrReadOnly):
		if s.pushDown.CompareAndSwap(false, true) {
			s.logf("tiered: remote tier is read-only, not pushing local builds upstream")
		}
	default:
		s.logf("tiered: remote %s failed for action %x: %v", what, actionID, err)
	}
}

// Get implements cas.Store. It checks Local first; a Local error is
// treated as a real failure and returned as-is, since a broken local disk
// is worth surfacing. A Local miss falls through to Remote -- a Remote
// error or miss degrades to a plain miss (ok=false, err=nil) rather than
// failing the Get, so a flaky shared cache never breaks a build that could
// otherwise just recompile the action itself. A Remote hit is written
// through to Local before returning, so the next Get for the same action
// is served purely locally.
func (s *Store) Get(actionID []byte) (cas.Entry, bool, error) {
	entry, ok, err := s.Local.Get(actionID)
	if err != nil {
		return cas.Entry{}, false, err
	}
	if ok || s.remoteDown.Load() {
		return entry, ok, nil
	}

	remoteEntry, body, remoteOK, remoteErr := s.fetchRemote(actionID)
	if remoteErr != nil {
		s.remoteFailed("get", actionID, remoteErr)
		return cas.Entry{}, false, nil
	}
	if !remoteOK {
		return cas.Entry{}, false, nil
	}

	if body != nil {
		// The bytes are already in hand: write them straight into Local.
		// With no remote file to fall back on, a Local failure here is a
		// miss -- the go command will just rebuild the action.
		localPath, err := s.Local.Put(actionID, remoteEntry.OutputID, body, remoteEntry.Size)
		if err != nil {
			s.logf("tiered: writing remote hit for action %x into local failed, treating as a miss: %v", actionID, err)
			return cas.Entry{}, false, nil
		}
		remoteEntry.DiskPath = localPath
		return remoteEntry, true, nil
	}

	f, err := os.Open(remoteEntry.DiskPath)
	if err != nil {
		// Most likely evicted by the server between its Get and ours. The
		// go command can't use a path we couldn't open either, so this is
		// a miss, not a hit to pass along.
		s.logf("tiered: opening remote hit for action %x failed, treating as a miss: %v", actionID, err)
		return cas.Entry{}, false, nil
	}
	defer f.Close()

	localPath, err := s.Local.Put(actionID, remoteEntry.OutputID, f, remoteEntry.Size)
	if err != nil {
		s.logf("tiered: writing remote hit for action %x into local, serving from remote instead: %v", actionID, err)
		return remoteEntry, true, nil
	}
	remoteEntry.DiskPath = localPath
	return remoteEntry, true, nil
}

func (s *Store) fetchRemote(actionID []byte) (cas.Entry, io.Reader, bool, error) {
	if f, ok := s.Remote.(cas.Fetcher); ok {
		return f.Fetch(actionID)
	}
	entry, ok, err := s.Remote.Get(actionID)
	return entry, nil, ok, err
}

// Put implements cas.Store. It always writes to Local first and returns
// that outcome -- a Local write failure fails the Put, matching a plain
// disk.Store's own contract. Pushing the same bytes to Remote, so the
// shared cache benefits too, happens in the background: the go command
// gets its answer as soon as the local write lands, without waiting on a
// network round trip and upload per object. Close (or Flush) waits for
// queued uploads to finish. A Remote failure is logged and otherwise
// ignored, since the build has already succeeded locally.
func (s *Store) Put(actionID, outputID []byte, body io.Reader, size int64) (string, error) {
	localPath, err := s.Local.Put(actionID, outputID, body, size)
	if err != nil {
		return "", err
	}
	if s.remoteDown.Load() || s.pushDown.Load() {
		return localPath, nil
	}

	s.closeMu.RLock()
	defer s.closeMu.RUnlock()
	if s.closed {
		return localPath, nil
	}
	s.startOnce.Do(s.startWorker)
	s.pending.Add(1)
	s.uploads <- upload{actionID: actionID, outputID: outputID, localPath: localPath, size: size}
	return localPath, nil
}

func (s *Store) startWorker() {
	s.uploads = make(chan upload, uploadQueueSize)
	s.workerWG.Add(1)
	go func() {
		defer s.workerWG.Done()
		for u := range s.uploads {
			s.push(u)
			s.pending.Done()
		}
	}()
}

// push uploads one finished Put to Remote, reopening Local's just-written
// file rather than having buffered the body twice.
func (s *Store) push(u upload) {
	if s.remoteDown.Load() || s.pushDown.Load() {
		return
	}
	f, err := os.Open(u.localPath)
	if err != nil {
		s.logf("tiered: reopening %s to push action %x upstream: %v", u.localPath, u.actionID, err)
		return
	}
	defer f.Close()
	if _, err := s.Remote.Put(u.actionID, u.outputID, f, u.size); err != nil {
		s.remoteFailed("put", u.actionID, err)
	}
}

// Flush blocks until every upload queued by an earlier Put has finished
// (or failed).
func (s *Store) Flush() {
	s.pending.Wait()
}

// Close implements cas.Store: it waits for queued uploads to finish, then
// closes both tiers. Both are attempted even if one fails; the first error
// encountered is returned.
func (s *Store) Close() error {
	s.closeMu.Lock()
	alreadyClosed := s.closed
	s.closed = true
	if !alreadyClosed && s.uploads != nil {
		close(s.uploads)
	}
	s.closeMu.Unlock()
	s.workerWG.Wait()

	localErr := s.Local.Close()
	remoteErr := s.Remote.Close()
	if localErr != nil {
		return fmt.Errorf("closing local tier: %w", localErr)
	}
	if remoteErr != nil {
		return fmt.Errorf("closing remote tier: %w", remoteErr)
	}
	return nil
}
