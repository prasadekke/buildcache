// Package metrics defines the hook buildcache calls out to for hit-rate and
// latency data, plus a stdlib-only in-memory implementation that backs
// cacheserver's built-in dashboard.
//
// buildcache doesn't import an OpenTelemetry SDK, or any other metrics
// backend, itself — Recorder is the seam instead. Anyone wanting OTel,
// StatsD, or anything else implements Recorder against their own client
// and passes it to WrapHandler in place of the built-in Counters; the rest
// of buildcache never needs to know which one it's talking to.
package metrics

import "time"

// Recorder receives outcome and latency data for every Get/Put a wrapped
// protocol.Handler serves (see WrapHandler). Implementations must be safe
// for concurrent use, since a single Handler may serve many connections at
// once (see cmd/cacheserver).
type Recorder interface {
	// Get records the result of one "get" request: hit reports whether it
	// was a cache hit, and dur is how long the underlying Store.Get call
	// took.
	Get(hit bool, dur time.Duration)

	// Put records one "put" request: size is the stored body's byte
	// count, and dur is how long the underlying Store.Put call took.
	Put(size int64, dur time.Duration)

	// Error records that a Get or Put returned an error rather than an
	// ordinary hit/miss/store outcome. op is "get" or "put".
	Error(op string)
}

// Nop is a Recorder that discards everything it's given. It's the natural
// zero value to embed wherever a Recorder is optional.
var Nop Recorder = nopRecorder{}

type nopRecorder struct{}

func (nopRecorder) Get(hit bool, dur time.Duration)   {}
func (nopRecorder) Put(size int64, dur time.Duration) {}
func (nopRecorder) Error(op string)                   {}
