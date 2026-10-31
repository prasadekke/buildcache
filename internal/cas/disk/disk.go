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
// unbounded. For an entry with action ID "ab12..." and output ID "cd34...":
//
//	<root>/ab/ab12....meta         -- JSON: {"OutputID": "<hex>", "Time": "...", "Body": "ab12...-cd34....body"}
//	<root>/ab/ab12...-cd34....body -- the raw bytes passed to Put
//
// The body is named for its (action, output) pair, and the meta records
// which body it describes, so a meta can never end up pointing at another
// Put's bytes: two concurrent Puts of the same action with different
// outputs write two different body files, and whichever meta lands last
// still names its own. (With a single "<action>.body" per action, the two
// writes could interleave as body A, body B, meta B, meta A -- leaving
// meta A's OutputID attached to body B's content.) A meta without a Body
// field, from before this layout, refers to the legacy "<action>.body".
//
// A single Store is safe to share across goroutines and across separate
// OS processes: all writes go through writeAtomic (temp file + rename), so
// concurrent Gets never observe a partially written entry.
type Store struct {
	root string

	// EvictGrace protects anything modified more recently than this from
	// Evict: entries used within it (Get bumps a body's mtime), and
	// orphaned or temporary files that may simply belong to a Put still in
	// progress. Without it, a sweep that runs between a Put's body and
	// meta writes would see a body with no meta yet and delete it out from
	// under the Put that's about to return its path. New sets it to
	// DefaultEvictGrace.
	EvictGrace time.Duration
}

// DefaultEvictGrace is EvictGrace's default. The go command's own guidance
// (cmd/go/internal/cache.Cache's Close doc) is never to trim anything used
// recently by another process, since it may still be about to read a path
// it was just handed.
const DefaultEvictGrace = 10 * time.Minute

// New returns a Store rooted at dir, creating dir if needed.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating cache root %q: %w", dir, err)
	}
	return &Store{root: dir, EvictGrace: DefaultEvictGrace}, nil
}

type meta struct {
	OutputID string
	Time     time.Time

	// Body is the body file's name within the shard directory. Empty in
	// metas written before bodies were named per output; see bodyName.
	Body string `json:",omitempty"`
}

const (
	bodySuffix = ".body"
	metaSuffix = ".meta"
	tmpPrefix  = ".tmp-"
)

func (s *Store) shardDir(hexID string) string {
	shard := "00"
	if len(hexID) >= 2 {
		shard = hexID[:2]
	}
	return filepath.Join(s.root, shard)
}

func (s *Store) metaPath(actionID []byte) string {
	hexID := hex.EncodeToString(actionID)
	return filepath.Join(s.shardDir(hexID), hexID+metaSuffix)
}

func (s *Store) bodyPath(actionID, outputID []byte) string {
	hexID := hex.EncodeToString(actionID)
	return filepath.Join(s.shardDir(hexID), hexID+"-"+hex.EncodeToString(outputID)+bodySuffix)
}

// legacyBodyPath is where bodies lived before they were named per output.
func (s *Store) legacyBodyPath(actionID []byte) string {
	hexID := hex.EncodeToString(actionID)
	return filepath.Join(s.shardDir(hexID), hexID+bodySuffix)
}

// bodyName returns the file name (within its shard) of the body that m,
// stored as the meta for the action with hex ID hexID, describes.
func (m meta) bodyName(hexID string) string {
	if m.Body != "" {
		return m.Body
	}
	return hexID + bodySuffix
}

// Get implements cas.Store.
func (s *Store) Get(actionID []byte) (cas.Entry, bool, error) {
	metaPath := s.metaPath(actionID)

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
	if m.Body != "" && (filepath.Base(m.Body) != m.Body || !strings.HasSuffix(m.Body, bodySuffix)) {
		return cas.Entry{}, false, fmt.Errorf("meta for action %x names an invalid body file %q", actionID, m.Body)
	}
	bodyPath := filepath.Join(filepath.Dir(metaPath), m.bodyName(hex.EncodeToString(actionID)))

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
	if body == nil {
		// The protocol layer always supplies a valid, possibly-empty
		// Reader for a zero-byte body (see internal/protocol's readLine
		// doc comment), but guard here too: a nil Reader reaching io.Copy
		// panics, and a panic inside cacheserver's per-connection goroutine
		// takes down the whole daemon, not just the one bad request.
		body = bytes.NewReader(nil)
	}
	bodyPath, metaPath := s.bodyPath(actionID, outputID), s.metaPath(actionID)
	if err := os.MkdirAll(filepath.Dir(bodyPath), 0o755); err != nil {
		return "", fmt.Errorf("creating shard dir: %w", err)
	}

	if err := writeAtomic(bodyPath, body); err != nil {
		return "", fmt.Errorf("writing body for action %x: %w", actionID, err)
	}

	m := meta{
		OutputID: hex.EncodeToString(outputID),
		Time:     time.Now().UTC(),
		Body:     filepath.Base(bodyPath),
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
// Regardless of maxBytes, it also removes debris: bodies no meta refers to
// (left by a process killed mid-Put, or superseded by a later Put of the
// same action with a different output), metas whose body is gone or that
// can't be parsed, and temp files abandoned by writeAtomic. Nothing
// modified within EvictGrace is touched, whether debris or not -- see
// EvictGrace for why.
func (s *Store) Evict(maxBytes int64) (freedBytes int64, err error) {
	type body struct {
		path  string
		mtime time.Time
		size  int64
		// metaPath is the meta that refers to this body, if any.
		metaPath string
	}

	var (
		bodies = map[string]*body{} // keyed by path
		metas  []string
		cutoff = time.Now().Add(-s.EvictGrace)
	)

	removeIfStale := func(path string, info os.FileInfo) bool {
		if info.ModTime().After(cutoff) {
			return false
		}
		return os.Remove(path) == nil
	}

	walkErr := filepath.WalkDir(s.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil // removed concurrently mid-walk
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		switch {
		case strings.HasPrefix(name, tmpPrefix):
			if info, err := d.Info(); err == nil && removeIfStale(path, info) {
				freedBytes += info.Size()
			}
		case strings.HasSuffix(name, metaSuffix):
			metas = append(metas, path)
		case strings.HasSuffix(name, bodySuffix):
			info, err := d.Info()
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("stat %s: %w", path, err)
			}
			bodies[path] = &body{path: path, mtime: info.ModTime(), size: info.Size()}
		}
		return nil
	})
	if walkErr != nil {
		return freedBytes, fmt.Errorf("walking store at %s: %w", s.root, walkErr)
	}

	// Pair each meta with the body it names.
	for _, metaPath := range metas {
		info, err := os.Stat(metaPath)
		if err != nil {
			continue
		}
		var m meta
		mb, err := os.ReadFile(metaPath)
		if err == nil {
			err = json.Unmarshal(mb, &m)
		}
		if err != nil {
			removeIfStale(metaPath, info) // unparseable: Get would only ever error on it
			continue
		}
		hexID := strings.TrimSuffix(filepath.Base(metaPath), metaSuffix)
		b, ok := bodies[filepath.Join(filepath.Dir(metaPath), m.bodyName(hexID))]
		if !ok {
			removeIfStale(metaPath, info) // its body is gone: Get already treats it as a miss
			continue
		}
		b.metaPath = metaPath
	}

	var (
		live  []*body
		total int64
	)
	for _, b := range bodies {
		if b.metaPath == "" {
			if !b.mtime.After(cutoff) && os.Remove(b.path) == nil {
				freedBytes += b.size
			}
			continue
		}
		live = append(live, b)
		total += b.size
	}

	if total <= maxBytes {
		return freedBytes, nil
	}

	sort.Slice(live, func(i, j int) bool { return live[i].mtime.Before(live[j].mtime) })

	for _, b := range live {
		if total <= maxBytes {
			break
		}
		if b.mtime.After(cutoff) {
			// Everything from here on was used within the grace period;
			// leave the store over its cap rather than evict an object
			// some build may be about to read.
			break
		}
		if err := os.Remove(b.path); err != nil && !os.IsNotExist(err) {
			// Leave it for the next sweep rather than failing the whole
			// pass over one uncooperative file.
			continue
		}
		s.removeMetaIfFor(b.metaPath, filepath.Base(b.path))
		total -= b.size
		freedBytes += b.size
	}

	return freedBytes, nil
}

// removeMetaIfFor removes the meta at metaPath if it still refers to the
// body named bodyName. A concurrent Put may have just replaced it with a
// meta for a new body, which must be left alone. (Best-effort either way:
// a dangling meta is harmless, Get treats it as a miss.)
func (s *Store) removeMetaIfFor(metaPath, bodyName string) {
	mb, err := os.ReadFile(metaPath)
	if err != nil {
		return
	}
	var m meta
	if json.Unmarshal(mb, &m) != nil {
		return
	}
	if m.bodyName(strings.TrimSuffix(filepath.Base(metaPath), metaSuffix)) == bodyName {
		os.Remove(metaPath)
	}
}

// writeAtomic writes r to a temp file beside path, then renames it into
// place, so a concurrent Get never observes a partially written entry.
func writeAtomic(path string, r io.Reader) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, tmpPrefix+"*")
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
