// Package cas defines the storage interface behind every cache backend
// (disk, and later S3) and the adapter that bridges it to the
// GOCACHEPROG wire protocol.
//
// Splitting this out from internal/protocol keeps backends decoupled from
// the wire format: a backend only implements Store (four methods, no
// JSON/base64 in sight), and Handler in this package does the adapting
// once, for all of them.
package cas

import (
	"errors"
	"io"
	"time"
)

// ErrUnavailable marks an error from a Store whose backend can no longer
// be reached at all (a dead or desynced connection), as opposed to one
// that failed a single request. Callers layering Stores (see
// internal/cas/tiered) use it to stop consulting that backend for the rest
// of its lifetime instead of failing slowly on every remaining call.
var ErrUnavailable = errors.New("store unavailable")

// ErrReadOnly marks a Put rejected because the Store only permits reads.
var ErrReadOnly = errors.New("store is read-only")

// Fetcher is optionally implemented by Stores whose hits can arrive as
// bytes in memory rather than as a local file (see internal/cas/remote).
// Fetch is like Get, except that on a hit it may return the content as
// body with entry.DiskPath left empty, so a caller that's about to copy
// the bytes into another Store anyway (internal/cas/tiered) can skip
// materializing a temporary file first. When body is nil, entry.DiskPath
// is set exactly as Get would set it.
type Fetcher interface {
	Fetch(actionID []byte) (entry Entry, body io.Reader, ok bool, err error)
}

// Entry is what a Store returns on a cache hit.
type Entry struct {
	OutputID []byte
	Size     int64
	Time     time.Time

	// DiskPath is a local filesystem path where the entry's bytes can be
	// read from. For a disk-backed Store that's just where it wrote the
	// file. A future network-backed Store (S3) would need to materialize
	// a local copy here too — the go command needs an actual file path,
	// not a URL, since GOCACHEPROG's DiskPath contract is filesystem-only.
	DiskPath string
}

// Store is the storage interface every cache backend implements.
type Store interface {
	// Get looks up actionID. ok is false on a cache miss; err is non-nil
	// only for an actual failure (I/O error, corrupt metadata, etc.), not
	// for an ordinary miss.
	Get(actionID []byte) (entry Entry, ok bool, err error)

	// Put stores body (size bytes) under actionID, tagged with outputID,
	// and returns the local path the bytes now live at.
	Put(actionID, outputID []byte, body io.Reader, size int64) (diskPath string, err error)

	// Close releases any resources this Store is holding (file handles,
	// network connections). Callers own the Store's lifetime and are
	// responsible for calling Close when they're done with it — Store
	// implementations must not close themselves on any particular
	// request, since a single Store may be shared across many concurrent
	// callers (see cmd/cacheserver).
	Close() error
}
