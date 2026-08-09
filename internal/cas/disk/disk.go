// Package disk implements a local-filesystem-backed cas.Store. It's the v1
// storage backend: correct and simple, with no network dependency, meant
// to validate the protocol plumbing before an S3-backed cas.Store is added
// alongside it.
package disk

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/prasadekke/buildcache/internal/cas"
)

// Store writes cache entries under a root directory, sharded by the first
// byte of the action ID's hex encoding so no single directory grows
// unbounded. For an entry with action ID "ab12...":
//
//	<root>/ab/ab12....body  -- the raw bytes passed to Put
//	<root>/ab/ab12....meta  -- JSON: {"OutputID": "<hex>", "Time": "..."}
//
// A single Store is safe to share across goroutines and across separate
// OS processes: all writes go through writeAtomic (temp file + rename), so
// concurrent Gets never observe a partially written entry.
type Store struct {
	root string
}

// New returns a Store rooted at dir, creating dir if needed.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating cache root %q: %w", dir, err)
	}
	return &Store{root: dir}, nil
}

type meta struct {
	OutputID string
	Time     time.Time
}

func (s *Store) paths(actionID []byte) (bodyPath, metaPath string) {
	hexID := hex.EncodeToString(actionID)
	shard := "00"
	if len(hexID) >= 2 {
		shard = hexID[:2]
	}
	dir := filepath.Join(s.root, shard)
	return filepath.Join(dir, hexID+".body"), filepath.Join(dir, hexID+".meta")
}

// Get implements cas.Store.
func (s *Store) Get(actionID []byte) (cas.Entry, bool, error) {
	bodyPath, metaPath := s.paths(actionID)

	mb, err := os.ReadFile(metaPath)
	if os.IsNotExist(err) {
		return cas.Entry{}, false, nil
	}
	if err != nil {
		return cas.Entry{}, false, fmt.Errorf("reading meta for action %x: %w", actionID, err)
	}
	var m meta
	if err := json.Unmarshal(mb, &m); err != nil {
		return cas.Entry{}, false, fmt.Errorf("decoding meta for action %x: %w", actionID, err)
	}

	fi, err := os.Stat(bodyPath)
	if os.IsNotExist(err) {
		// meta exists but body doesn't: treat as a miss rather than an
		// error, so a partially-written or manually-cleaned entry doesn't
		// wedge every subsequent build that hits this key.
		return cas.Entry{}, false, nil
	}
	if err != nil {
		return cas.Entry{}, false, fmt.Errorf("stat body for action %x: %w", actionID, err)
	}

	outputID, err := hex.DecodeString(m.OutputID)
	if err != nil {
		return cas.Entry{}, false, fmt.Errorf("decoding stored output ID: %w", err)
	}

	// Bump the body's mtime on every hit so it doubles as a last-used
	// timestamp for Evict's LRU ordering, without needing platform-specific
	// atime syscalls (real atime tracking is disabled by default on most
	// modern filesystems anyway). Best-effort: a failure here shouldn't
	// fail the cache hit itself.
	now := time.Now()
	_ = os.Chtimes(bodyPath, now, now)

	return cas.Entry{
		OutputID: outputID,
		Size:     fi.Size(),
		Time:     m.Time,
		DiskPath: bodyPath,
	}, true, nil
}

// Put implements cas.Store.
func (s *Store) Put(actionID, outputID []byte, body io.Reader, size int64) (string, error) {
	bodyPath, metaPath := s.paths(actionID)
	if err := os.MkdirAll(filepath.Dir(bodyPath), 0o755); err != nil {
		return "", fmt.Errorf("creating shard dir: %w", err)
	}

	if err := writeAtomic(bodyPath, body); err != nil {
		return "", fmt.Errorf("writing body for action %x: %w", actionID, err)
	}

	m := meta{
		OutputID: hex.EncodeToString(outputID),
		Time:     time.Now().UTC(),
	}
	mb, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("encoding meta: %w", err)
	}
	if err := writeAtomic(metaPath, bytes.NewReader(mb)); err != nil {
		return "", fmt.Errorf("writing meta for action %x: %w", actionID, err)
	}

	return bodyPath, nil
}

// Close implements cas.Store. Nothing to release for a disk-backed store
// today; kept so callers (cmd/cacheprog, cmd/cacheserver) can defer
// store.Close() uniformly across backends, including future ones that do
// hold real resources (S3 client connections, etc).
func (s *Store) Close() error { return nil }

// Evict walks the entire store and removes least-recently-used entries
// (oldest body mtime first, see the comment in Get) until the total size
// of remaining bodies is at or below maxBytes. It returns the number of
// bytes freed.
//
// This is a full directory walk, so it's not cheap — appropriate for a
// periodic background sweep in a long-lived process (see cmd/cacheserver's
// GC ticker), not for calling on every request. cmd/cacheprog's direct
// mode (one process per `go build` invocation) exposes eviction as an
// opt-in "-gc" flag instead, for exactly this reason: running a full walk
// on every subprocess would slow down every build.
//
// A meta file without a matching body (or vice versa) is treated as
// orphaned and removed unconditionally, regardless of maxBytes — that
// state should only arise from a process being killed mid-Put, and
// leaving it around serves no purpose.
func (s *Store) Evict(maxBytes int64) (freedBytes int64, err error) {
	type item struct {
		bodyPath, metaPath string
		mtime              time.Time
		size               int64
	}

	var (
		items []item
		total int64
	)

	walkErr := filepath.WalkDir(s.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		switch {
		case strings.HasSuffix(path, ".meta"):
			bodyPath := strings.TrimSuffix(path, ".meta") + ".body"
			if _, err := os.Stat(bodyPath); os.IsNotExist(err) {
				// Orphaned meta with no body: harmless (Get treats it as
				// a miss) but pointless to keep, and we're walking the
				// tree anyway.
				os.Remove(path)
			}
			return nil

		case strings.HasSuffix(path, ".body"):
			info, err := d.Info()
			if err != nil {
				return fmt.Errorf("stat %s: %w", path, err)
			}
			metaPath := strings.TrimSuffix(path, ".body") + ".meta"
			if _, err := os.Stat(metaPath); os.IsNotExist(err) {
				// Orphaned body with no meta: remove unconditionally,
				// regardless of maxBytes.
				if rmErr := os.Remove(path); rmErr == nil {
					freedBytes += info.Size()
				}
				return nil
			}
			items = append(items, item{
				bodyPath: path,
				metaPath: metaPath,
				mtime:    info.ModTime(),
				size:     info.Size(),
			})
			total += info.Size()
			return nil

		default:
			return nil
		}
	})
	if walkErr != nil {
		return freedBytes, fmt.Errorf("walking store at %s: %w", s.root, walkErr)
	}

	if total <= maxBytes {
		return freedBytes, nil
	}

	sort.Slice(items, func(i, j int) bool { return items[i].mtime.Before(items[j].mtime) })

	for _, it := range items {
		if total <= maxBytes {
			break
		}
		if err := os.Remove(it.bodyPath); err != nil && !os.IsNotExist(err) {
			// Leave it for the next sweep rather than failing the whole
			// pass over one uncooperative file.
			continue
		}
		os.Remove(it.metaPath) // best-effort; a dangling meta is harmless, Get treats it as a miss
		total -= it.size
		freedBytes += it.size
	}

	return freedBytes, nil
}

// writeAtomic writes r to a temp file beside path, then renames it into
// place, so a concurrent Get never observes a partially written entry.
func writeAtomic(path string, r io.Reader) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds

	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
