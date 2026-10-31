package cas

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"github.com/prasadekke/buildcache/internal/protocol"
)

func TestVerifyPuts_AcceptsMatchingOutputID(t *testing.T) {
	body := []byte("real build output")
	sum := sha256.Sum256(body)
	store := &fakeStore{putDiskPath: "/cache/ok"}
	h := VerifyPuts(NewHandler(store))

	req := &protocol.Request{ActionID: []byte("a"), OutputID: sum[:], Body: bytes.NewReader(body), BodySize: int64(len(body))}
	resp := &protocol.Response{}
	if err := h.Put(req, resp); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !bytes.Equal(store.gotBody, body) {
		t.Errorf("store received %q, want the full body %q (hashing must rewind it)", store.gotBody, body)
	}
}

func TestVerifyPuts_RejectsMismatch(t *testing.T) {
	store := &fakeStore{putDiskPath: "/cache/bad"}
	h := VerifyPuts(NewHandler(store))

	sum := sha256.Sum256([]byte("what the client claimed"))
	req := &protocol.Request{ActionID: []byte("a"), OutputID: sum[:], Body: bytes.NewReader([]byte("what it actually sent")), BodySize: 21}
	if err := h.Put(req, &protocol.Response{}); err == nil {
		t.Fatal("Put err = nil, want a hash mismatch rejection")
	}
	if store.gotBody != nil {
		t.Error("a mismatched put reached the store")
	}
}

func TestVerifyPuts_ZeroByteBody(t *testing.T) {
	sum := sha256.Sum256(nil)
	h := VerifyPuts(NewHandler(&fakeStore{putDiskPath: "/cache/empty"}))
	req := &protocol.Request{ActionID: []byte("a"), OutputID: sum[:], Body: bytes.NewReader(nil)}
	if err := h.Put(req, &protocol.Response{}); err != nil {
		t.Fatalf("Put of a valid empty object: %v", err)
	}
}
