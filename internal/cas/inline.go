package cas

import (
	"fmt"
	"os"

	"github.com/prasadekke/buildcache/internal/protocol"
)

// InlineGetBodies wraps h so every "get" hit's response carries the
// object's bytes inline (Response.Body/BodySize), not just DiskPath.
//
// DiskPath is only meaningful to a caller on the same machine as h's
// Store. That's guaranteed for the go-command-facing leg (a GOCACHEPROG
// subprocess is always local) and for cacheprog talking to cacheserver
// over a same-machine Unix socket, but not once cacheserver is reachable
// over the network: a remote cacheprog can't open a path on cacheserver's
// filesystem. cacheserver should wrap its handler with InlineGetBodies for
// exactly those connections -- see internal/cas/remote.Store, which reads
// Body when present and materializes a local copy instead of trusting
// DiskPath verbatim. Wrapping the same-machine Unix-socket handler with
// this too would just be wasted I/O (an extra read-and-resend of bytes
// that were already directly readable), so it should stay conditional on
// transport, not applied unconditionally.
func InlineGetBodies(h protocol.Handler) protocol.Handler {
	return &inlineHandler{h: h}
}

type inlineHandler struct {
	h protocol.Handler
}

func (w *inlineHandler) Get(req *protocol.Request, resp *protocol.Response) error {
	if err := w.h.Get(req, resp); err != nil {
		return err
	}
	if resp.Miss || resp.DiskPath == "" {
		return nil
	}

	// Hand over the open file rather than its contents: protocol.Server
	// streams it onto the wire and closes it, so a large object never has
	// to sit in memory whole. The size comes from the open file itself, not
	// resp.Size, so it always matches exactly what will be streamed even
	// if the path is replaced by a concurrent Put in the meantime.
	f, err := os.Open(resp.DiskPath)
	if err != nil {
		return fmt.Errorf("opening %s to inline in response: %w", resp.DiskPath, err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("stat %s to inline in response: %w", resp.DiskPath, err)
	}
	resp.Body = f
	resp.BodyInline = true
	resp.BodySize = fi.Size()
	return nil
}

func (w *inlineHandler) Put(req *protocol.Request, resp *protocol.Response) error {
	return w.h.Put(req, resp)
}
