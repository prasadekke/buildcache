package cas

import "github.com/prasad-ekke/buildcache/internal/protocol"

// Handler adapts a Store to protocol.Handler. It deliberately does not
// implement Close: a Handler is often built around a Store that outlives
// any single connection (see cmd/cacheserver, where one Store is shared
// across many concurrent GOCACHEPROG connections), so closing it in
// response to any one connection's "close" request would break every
// other connection sharing it. Callers that own a Store's whole lifetime
// (see cmd/cacheprog's direct mode) close it themselves once, explicitly.
type Handler struct {
	Store Store
}

// NewHandler wraps s so it can be passed to protocol.NewServer.
func NewHandler(s Store) *Handler {
	return &Handler{Store: s}
}

// Get implements protocol.Handler.
func (h *Handler) Get(req *protocol.Request, resp *protocol.Response) error {
	entry, ok, err := h.Store.Get(req.ActionID)
	if err != nil {
		return err
	}
	if !ok {
		resp.Miss = true
		return nil
	}
	resp.OutputID = entry.OutputID
	resp.Size = entry.Size
	t := entry.Time
	resp.Time = &t
	resp.DiskPath = entry.DiskPath
	return nil
}

// Put implements protocol.Handler.
func (h *Handler) Put(req *protocol.Request, resp *protocol.Response) error {
	diskPath, err := h.Store.Put(req.ActionID, req.OutputID, req.Body, req.BodySize)
	if err != nil {
		return err
	}
	resp.DiskPath = diskPath
	return nil
}
