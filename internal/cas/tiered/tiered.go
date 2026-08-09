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
	"fmt"
	"io"
	"os"

	"github.com/prasadekke/buildcache/internal/cas"
)

// Store composes Local and Remote into one cas.Store. Both must be
// non-nil; if there's no remote tier to fall back to, callers should just
// use Local directly instead of wrapping it here.
type Store struct {
	Local  cas.Store
	Remote cas.Store

	// Logf receives a one-line message whenever Remote fails in a way
	// that's deliberately non-fatal (a flaky or unreachable shared cache
	// degrades to local-only behavior; it never fails the build). Defaults
	// to a no-op if nil.
	Logf func(format string, args ...any)
}

func (s *Store) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
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
	if ok {
		return entry, true, nil
	}

	remoteEntry, remoteOK, remoteErr := s.Remote.Get(actionID)
	if remoteErr != nil {
		s.logf("tiered: remote get failed for action %x, treating as a miss: %v", actionID, remoteErr)
		return cas.Entry{}, false, nil
	}
	if !remoteOK {
		return cas.Entry{}, false, nil
	}

	body, err := os.Open(remoteEntry.DiskPath)
	if err != nil {
		s.logf("tiered: opening remote hit for action %x, serving from remote instead: %v", actionID, err)
		return remoteEntry, true, nil
	}
	defer body.Close()

	localPath, err := s.Local.Put(actionID, remoteEntry.OutputID, body, remoteEntry.Size)
	if err != nil {
		s.logf("tiered: writing remote hit for action %x into local, serving from remote instead: %v", actionID, err)
		return remoteEntry, true, nil
	}

	entry = remoteEntry
	entry.DiskPath = localPath
	return entry, true, nil
}

// Put implements cas.Store. It always writes to Local first and returns
// that outcome -- a Local write failure fails the Put, matching a plain
// disk.Store's own contract. It then best-effort pushes the same bytes to
// Remote (reopening Local's just-written file rather than buffering the
// body twice) so the shared cache benefits too; a Remote failure here is
// logged and otherwise ignored, since the build has already succeeded
// locally by this point.
func (s *Store) Put(actionID, outputID []byte, body io.Reader, size int64) (string, error) {
	localPath, err := s.Local.Put(actionID, outputID, body, size)
	if err != nil {
		return "", err
	}

	f, err := os.Open(localPath)
	if err != nil {
		s.logf("tiered: reopening %s to push action %x upstream: %v", localPath, actionID, err)
		return localPath, nil
	}
	defer f.Close()

	if _, err := s.Remote.Put(actionID, outputID, f, size); err != nil {
		s.logf("tiered: pushing action %x to remote failed (kept locally): %v", actionID, err)
	}
	return localPath, nil
}

// Close implements cas.Store, closing both tiers. Both are attempted even
// if one fails; the first error encountered is returned.
func (s *Store) Close() error {
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
