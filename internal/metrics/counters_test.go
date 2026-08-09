package metrics

import (
	"sync"
	"testing"
	"time"
)

func TestCounters_Get(t *testing.T) {
	var c Counters
	c.Get(true, 10*time.Millisecond)
	c.Get(false, 20*time.Millisecond)
	c.Get(true, 5*time.Millisecond)

	s := c.Snapshot()
	if s.Gets != 3 {
		t.Errorf("Gets = %d, want 3", s.Gets)
	}
	if s.Hits != 2 {
		t.Errorf("Hits = %d, want 2", s.Hits)
	}
	if s.GetLatency != 35*time.Millisecond {
		t.Errorf("GetLatency = %v, want 35ms", s.GetLatency)
	}
	if got, want := s.HitRate(), 2.0/3.0; got != want {
		t.Errorf("HitRate() = %v, want %v", got, want)
	}
}

func TestCounters_Put(t *testing.T) {
	var c Counters
	c.Put(100, 10*time.Millisecond)
	c.Put(200, 30*time.Millisecond)

	s := c.Snapshot()
	if s.Puts != 2 {
		t.Errorf("Puts = %d, want 2", s.Puts)
	}
	if s.PutBytes != 300 {
		t.Errorf("PutBytes = %d, want 300", s.PutBytes)
	}
	if s.PutLatency != 40*time.Millisecond {
		t.Errorf("PutLatency = %v, want 40ms", s.PutLatency)
	}
}

func TestCounters_Error(t *testing.T) {
	var c Counters
	c.Error("get")
	c.Error("get")
	c.Error("put")
	c.Error("unknown-op") // should be ignored, not panic or miscount

	s := c.Snapshot()
	if s.GetErrors != 2 {
		t.Errorf("GetErrors = %d, want 2", s.GetErrors)
	}
	if s.PutErrors != 1 {
		t.Errorf("PutErrors = %d, want 1", s.PutErrors)
	}
}

func TestSnapshot_ZeroValueRates(t *testing.T) {
	var s Snapshot
	if s.HitRate() != 0 {
		t.Errorf("HitRate() on zero Snapshot = %v, want 0", s.HitRate())
	}
	if s.MeanGetLatency() != 0 {
		t.Errorf("MeanGetLatency() on zero Snapshot = %v, want 0", s.MeanGetLatency())
	}
	if s.MeanPutLatency() != 0 {
		t.Errorf("MeanPutLatency() on zero Snapshot = %v, want 0", s.MeanPutLatency())
	}
}

func TestSnapshot_MeanLatency(t *testing.T) {
	s := Snapshot{Gets: 4, GetLatency: 100 * time.Millisecond, Puts: 5, PutLatency: 50 * time.Millisecond}
	if got, want := s.MeanGetLatency(), 25*time.Millisecond; got != want {
		t.Errorf("MeanGetLatency() = %v, want %v", got, want)
	}
	if got, want := s.MeanPutLatency(), 10*time.Millisecond; got != want {
		t.Errorf("MeanPutLatency() = %v, want %v", got, want)
	}
}

func TestNop_DiscardsEverything(t *testing.T) {
	// Just needs to not panic and to be usable as a Recorder.
	var r Recorder = Nop
	r.Get(true, time.Second)
	r.Put(1024, time.Second)
	r.Error("get")
}

func TestCounters_ConcurrentSafety(t *testing.T) {
	var c Counters
	const n = 100
	var wg sync.WaitGroup
	wg.Add(n * 3)
	for i := 0; i < n; i++ {
		go func() { defer wg.Done(); c.Get(true, time.Millisecond) }()
		go func() { defer wg.Done(); c.Put(1, time.Millisecond) }()
		go func() { defer wg.Done(); c.Error("get") }()
	}
	wg.Wait()

	s := c.Snapshot()
	if s.Gets != n || s.Hits != n {
		t.Errorf("Gets=%d Hits=%d, want both %d", s.Gets, s.Hits, n)
	}
	if s.Puts != n {
		t.Errorf("Puts = %d, want %d", s.Puts, n)
	}
	if s.GetErrors != n {
		t.Errorf("GetErrors = %d, want %d", s.GetErrors, n)
	}
}
