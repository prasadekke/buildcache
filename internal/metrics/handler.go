package metrics

import (
	"time"

	"github.com/prasadekke/buildcache/internal/protocol"
)

// WrapHandler returns a protocol.Handler that delegates every Get/Put to h,
// reporting the outcome and latency to rec before returning. It's a plain
// decorator, so the same Recorder can wrap either cacheprog's direct-mode
// handler or cacheserver's shared one without either needing to know
// metrics exist.
func WrapHandler(h protocol.Handler, rec Recorder) protocol.Handler {
	return &wrapped{h: h, rec: rec}
}

type wrapped struct {
	h   protocol.Handler
	rec Recorder
}

// Get implements protocol.Handler.
func (w *wrapped) Get(req *protocol.Request, resp *protocol.Response) error {
	start := time.Now()
	if err := w.h.Get(req, resp); err != nil {
		w.rec.Error("get")
		return err
	}
	w.rec.Get(!resp.Miss, time.Since(start))
	return nil
}

// Put implements protocol.Handler.
func (w *wrapped) Put(req *protocol.Request, resp *protocol.Response) error {
	start := time.Now()
	if err := w.h.Put(req, resp); err != nil {
		w.rec.Error("put")
		return err
	}
	w.rec.Put(req.BodySize, time.Since(start))
	return nil
}
