package cas

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/prasadekke/buildcache/internal/protocol"
)

// fakeStore is a Store test double that records its calls and returns
// caller-configured results.
type fakeStore struct {
	getEntry Entry
	getOK    bool
	getErr   error

	putDiskPath string
	putErr      error

	gotActionID []byte
	gotOutputID []byte
	gotBody     []byte
	gotSize     int64
}

func (f *fakeStore) Get(actionID []byte) (Entry, bool, error) {
	f.gotActionID = actionID
	return f.getEntry, f.getOK, f.getErr
}

func (f *fakeStore) Put(actionID, outputID []byte, body io.Reader, size int64) (string, error) {
	f.gotActionID = actionID
	f.gotOutputID = outputID
	f.gotSize = size
	if body != nil {
		b, err := io.ReadAll(body)
		if err != nil {
			return "", err
		}
		f.gotBody = b
	}
	return f.putDiskPath, f.putErr
}

func (f *fakeStore) Close() error { return nil }

func TestHandler_GetHit(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	store := &fakeStore{
		getEntry: Entry{OutputID: []byte("out"), Size: 7, Time: now, DiskPath: "/cache/x"},
		getOK:    true,
	}
	h := NewHandler(store)

	req := &protocol.Request{ActionID: []byte("action")}
	resp := &protocol.Response{}
	if err := h.Get(req, resp); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.Miss {
		t.Error("resp.Miss = true, want false")
	}
	if string(resp.OutputID) != "out" || resp.Size != 7 || resp.DiskPath != "/cache/x" {
		t.Errorf("resp = %+v, want OutputID=out Size=7 DiskPath=/cache/x", resp)
	}
	if resp.Time == nil || !resp.Time.Equal(now) {
		t.Errorf("resp.Time = %v, want %v", resp.Time, now)
	}
	if string(store.gotActionID) != "action" {
		t.Errorf("Store.Get called with ActionID=%q, want %q", store.gotActionID, "action")
	}
}

func TestHandler_GetMiss(t *testing.T) {
	store := &fakeStore{getOK: false}
	h := NewHandler(store)

	resp := &protocol.Response{}
	if err := h.Get(&protocol.Request{ActionID: []byte("nope")}, resp); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !resp.Miss {
		t.Error("resp.Miss = false, want true")
	}
}

func TestHandler_GetError(t *testing.T) {
	wantErr := errors.New("disk exploded")
	store := &fakeStore{getErr: wantErr}
	h := NewHandler(store)

	err := h.Get(&protocol.Request{ActionID: []byte("x")}, &protocol.Response{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Get err = %v, want %v", err, wantErr)
	}
}

func TestHandler_Put(t *testing.T) {
	store := &fakeStore{putDiskPath: "/cache/new"}
	h := NewHandler(store)

	body := []byte("hello")
	req := &protocol.Request{
		ActionID: []byte("action"),
		OutputID: []byte("output"),
		Body:     bytes.NewReader(body),
		BodySize: int64(len(body)),
	}
	resp := &protocol.Response{}
	if err := h.Put(req, resp); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if resp.DiskPath != "/cache/new" {
		t.Errorf("resp.DiskPath = %q, want /cache/new", resp.DiskPath)
	}
	if string(store.gotBody) != "hello" {
		t.Errorf("Store.Put body = %q, want %q", store.gotBody, "hello")
	}
	if string(store.gotActionID) != "action" || string(store.gotOutputID) != "output" {
		t.Errorf("Store.Put called with ActionID=%q OutputID=%q", store.gotActionID, store.gotOutputID)
	}
}

func TestHandler_PutError(t *testing.T) {
	wantErr := errors.New("disk full")
	store := &fakeStore{putErr: wantErr}
	h := NewHandler(store)

	err := h.Put(&protocol.Request{Body: bytes.NewReader(nil)}, &protocol.Response{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Put err = %v, want %v", err, wantErr)
	}
}
