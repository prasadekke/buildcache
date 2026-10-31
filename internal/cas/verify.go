package cas

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"

	"github.com/prasadekke/buildcache/internal/protocol"
)

// CheckOutputID reads r to the end, reports whether its content hashes to
// outputID, and rewinds r to the start either way. The go command always
// computes a "put"'s OutputID as the SHA-256 of its body (see
// cmd/go/internal/cache.(*ProgCache).Put), so any mismatch means the bytes
// were corrupted or truncated somewhere between the build that produced
// them and here.
func CheckOutputID(outputID []byte, r io.ReadSeeker) error {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return fmt.Errorf("hashing content: %w", err)
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewinding content after hashing: %w", err)
	}
	if sum := h.Sum(nil); !bytes.Equal(sum, outputID) {
		return fmt.Errorf("content hash %x does not match OutputID %x", sum, outputID)
	}
	return nil
}

// VerifyPuts wraps h so every "put" is rejected unless its body hashes to
// its OutputID (see CheckOutputID). cacheserver applies it to every
// connection, so nothing corrupted in transit or by a client bug ever
// lands in the shared store where every other machine would read it back.
//
// It doesn't stop a malicious client with valid credentials from storing
// a well-formed but wrong output for an action -- that's an authorization
// question (see tlsconfig.ServerWithReadOnly), not an integrity one.
func VerifyPuts(h protocol.Handler) protocol.Handler {
	return &verifyHandler{h: h}
}

type verifyHandler struct {
	h protocol.Handler
}

func (v *verifyHandler) Get(req *protocol.Request, resp *protocol.Response) error {
	return v.h.Get(req, resp)
}

func (v *verifyHandler) Put(req *protocol.Request, resp *protocol.Response) error {
	// The protocol layer hands over put bodies as in-memory bytes.Readers,
	// so this is normally a hash-and-rewind, not another copy.
	var body io.Reader = bytes.NewReader(nil)
	if req.Body != nil {
		body = req.Body
	}
	rs, ok := body.(io.ReadSeeker)
	if !ok {
		data, err := io.ReadAll(body)
		if err != nil {
			return fmt.Errorf("reading put body for action %x: %w", req.ActionID, err)
		}
		rs = bytes.NewReader(data)
	}
	if err := CheckOutputID(req.OutputID, rs); err != nil {
		return fmt.Errorf("rejecting put for action %x: %w", req.ActionID, err)
	}
	req.Body = rs
	return v.h.Put(req, resp)
}
