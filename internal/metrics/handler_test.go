package metrics

import (
	"errors"
	"testing"
	"time"

	"github.com/prasadekke/buildcache/internal/protocol"
)

type stubHandler struct {
	getResp func(*protocol.Response)
	getErr  error
	putResp func(*protocol.Response)
	putErr  error
}

func (s *stubHandler) Get(req *protocol.Request, resp *protocol.Response) error {
	if s.getResp != nil {
		s.getResp(resp)
	}
	return s.getErr
}

func (s *stubHandler) Put(req *protocol.Request, resp *protocol.Response) error {
	if s.putResp != nil {
		s.putResp(resp)
	}
	return s.putErr
}

func TestWrapHandler_RecordsHit(t *testing.T) {
	inner := &stubHandler{getResp: func(r *protocol.Response) { r.Miss = false }}
	rec := &Counters{}
	h := WrapHandler(inner, rec)

	if err := h.Get(&protocol.Request{}, &protocol.Response{}); err != nil {
		t.Fatalf("Get: %v", err)
	}
	s := rec.Snapshot()
	if s.Gets != 1 || s.Hits != 1 {
		t.Errorf("Gets=%d Hits=%d, want 1,1", s.Gets, s.Hits)
	}
}

func TestWrapHandler_RecordsMiss(t *testing.T) {
	inner := &stubHandler{getResp: func(r *protocol.Response) { r.Miss = true }}
	rec := &Counters{}
	h := WrapHandler(inner, rec)

	if err := h.Get(&protocol.Request{}, &protocol.Response{}); err != nil {
		t.Fatalf("Get: %v", err)
	}
	s := rec.Snapshot()
	if s.Gets != 1 || s.Hits != 0 {
		t.Errorf("Gets=%d Hits=%d, want 1,0", s.Gets, s.Hits)
	}
}

func TestWrapHandler_RecordsPut(t *testing.T) {
	inner := &stubHandler{}
	rec := &Counters{}
	h := WrapHandler(inner, rec)

	req := &protocol.Request{BodySize: 512}
	if err := h.Put(req, &protocol.Response{}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	s := rec.Snapshot()
	if s.Puts != 1 || s.PutBytes != 512 {
		t.Errorf("Puts=%d PutBytes=%d, want 1,512", s.Puts, s.PutBytes)
	}
}

func TestWrapHandler_PropagatesGetError(t *testing.T) {
	wantErr := errors.New("boom")
	inner := &stubHandler{getErr: wantErr}
	rec := &Counters{}
	h := WrapHandler(inner, rec)

	err := h.Get(&protocol.Request{}, &protocol.Response{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Get err = %v, want %v", err, wantErr)
	}
	s := rec.Snapshot()
	if s.GetErrors != 1 {
		t.Errorf("GetErrors = %d, want 1", s.GetErrors)
	}
	if s.Gets != 0 {
		t.Errorf("Gets = %d, want 0 (an error is not a hit or a miss)", s.Gets)
	}
}

func TestWrapHandler_PropagatesPutError(t *testing.T) {
	wantErr := errors.New("disk full")
	inner := &stubHandler{putErr: wantErr}
	rec := &Counters{}
	h := WrapHandler(inner, rec)

	err := h.Put(&protocol.Request{}, &protocol.Response{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Put err = %v, want %v", err, wantErr)
	}
	s := rec.Snapshot()
	if s.PutErrors != 1 {
		t.Errorf("PutErrors = %d, want 1", s.PutErrors)
	}
	if s.Puts != 0 {
		t.Errorf("Puts = %d, want 0", s.Puts)
	}
}

func TestWrapHandler_LatencyIsMeasured(t *testing.T) {
	inner := &stubHandler{getResp: func(r *protocol.Response) {
		time.Sleep(5 * time.Millisecond)
	}}
	rec := &Counters{}
	h := WrapHandler(inner, rec)
	if err := h.Get(&protocol.Request{}, &protocol.Response{}); err != nil {
		t.Fatalf("Get: %v", err)
	}

	s := rec.Snapshot()
	if s.GetLatency < 5*time.Millisecond {
		t.Errorf("GetLatency = %v, want at least 5ms", s.GetLatency)
	}
}
