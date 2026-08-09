package metrics

import (
	"sync/atomic"
	"time"
)

// Counters is an in-memory Recorder that accumulates request counts, hit
// rate, and total latency. It's the default: cheap enough to run
// unconditionally (cmd/cacheprog's -stats, cmd/cacheserver always), and it
// backs the "/" dashboard and "/metrics" Prometheus endpoint that
// cacheserver serves when -metrics-addr is set.
//
// All fields are touched only through sync/atomic, so a *Counters needs no
// lock and is safe to share across every connection a Handler serves.
type Counters struct {
	gets      atomic.Int64
	hits      atomic.Int64
	getErrors atomic.Int64
	getNanos  atomic.Int64

	puts      atomic.Int64
	putBytes  atomic.Int64
	putErrors atomic.Int64
	putNanos  atomic.Int64
}

// Get implements Recorder.
func (c *Counters) Get(hit bool, dur time.Duration) {
	c.gets.Add(1)
	if hit {
		c.hits.Add(1)
	}
	c.getNanos.Add(dur.Nanoseconds())
}

// Put implements Recorder.
func (c *Counters) Put(size int64, dur time.Duration) {
	c.puts.Add(1)
	c.putBytes.Add(size)
	c.putNanos.Add(dur.Nanoseconds())
}

// Error implements Recorder.
func (c *Counters) Error(op string) {
	switch op {
	case "get":
		c.getErrors.Add(1)
	case "put":
		c.putErrors.Add(1)
	}
}

// Snapshot is a point-in-time read of a Counters. Because it's copied out
// from the individually-atomic fields rather than read under one lock, it's
// not a perfectly consistent instant across all fields under heavy
// concurrent traffic — fine for a dashboard refreshed every few seconds,
// not a guarantee to build correctness logic on.
type Snapshot struct {
	Gets, Hits, GetErrors int64
	GetLatency            time.Duration // summed across all gets; divide by Gets for the mean

	Puts, PutErrors int64
	PutBytes        int64
	PutLatency      time.Duration // summed across all puts; divide by Puts for the mean
}

// HitRate returns Hits/Gets as a fraction in [0,1], or 0 if there have been
// no gets yet.
func (s Snapshot) HitRate() float64 {
	if s.Gets == 0 {
		return 0
	}
	return float64(s.Hits) / float64(s.Gets)
}

// MeanGetLatency returns GetLatency/Gets, or 0 if there have been no gets.
func (s Snapshot) MeanGetLatency() time.Duration {
	if s.Gets == 0 {
		return 0
	}
	return s.GetLatency / time.Duration(s.Gets)
}

// MeanPutLatency returns PutLatency/Puts, or 0 if there have been no puts.
func (s Snapshot) MeanPutLatency() time.Duration {
	if s.Puts == 0 {
		return 0
	}
	return s.PutLatency / time.Duration(s.Puts)
}

// Snapshot returns the current counter values.
func (c *Counters) Snapshot() Snapshot {
	return Snapshot{
		Gets:       c.gets.Load(),
		Hits:       c.hits.Load(),
		GetErrors:  c.getErrors.Load(),
		GetLatency: time.Duration(c.getNanos.Load()),
		Puts:       c.puts.Load(),
		PutErrors:  c.putErrors.Load(),
		PutBytes:   c.putBytes.Load(),
		PutLatency: time.Duration(c.putNanos.Load()),
	}
}
