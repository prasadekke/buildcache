package cas

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/prasadekke/buildcache/internal/protocol"
)

func TestInlineGetBodies_HitReadsDiskPathIntoBody(t *testing.T) {
	dir := t.TempDir()
	diskPath := filepath.Join(dir, "object")
	want := []byte("bytes that live on cacheserver's disk")
	if err := os.WriteFile(diskPath, want, 0o644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	store := &fakeStore{getEntry: Entry{OutputID: []byte("out"), Size: int64(len(want)), DiskPath: diskPath}, getOK: true}
	h := InlineGetBodies(NewHandler(store))

	resp := &protocol.Response{}
	if err := h.Get(&protocol.Request{ActionID: []byte("a")}, resp); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.DiskPath != diskPath {
		t.Errorf("resp.DiskPath = %q, want %q (still set, for a same-machine caller that doesn't need Body)", resp.DiskPath, diskPath)
	}
	if !resp.BodyInline {
		t.Error("resp.BodyInline = false, want true")
	}
	if resp.BodySize != int64(len(want)) {
		t.Errorf("resp.BodySize = %d, want %d", resp.BodySize, len(want))
	}
	if resp.Body == nil {
		t.Fatal("resp.Body is nil, want the file's contents inlined")
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading resp.Body: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("resp.Body = %q, want %q", got, want)
	}
}

func TestInlineGetBodies_Miss(t *testing.T) {
	store := &fakeStore{getOK: false}
	h := InlineGetBodies(NewHandler(store))

	resp := &protocol.Response{}
	if err := h.Get(&protocol.Request{ActionID: []byte("a")}, resp); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !resp.Miss {
		t.Error("resp.Miss = false, want true")
	}
	if resp.Body != nil || resp.BodyInline || resp.BodySize != 0 {
		t.Errorf("resp.Body=%v resp.BodyInline=%v resp.BodySize=%d, want all zero for a miss", resp.Body, resp.BodyInline, resp.BodySize)
	}
}

func TestInlineGetBodies_UnderlyingErrorPropagates(t *testing.T) {
	wantErr := errors.New("disk exploded")
	store := &fakeStore{getErr: wantErr}
	h := InlineGetBodies(NewHandler(store))

	err := h.Get(&protocol.Request{ActionID: []byte("a")}, &protocol.Response{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Get err = %v, want %v", err, wantErr)
	}
}

func TestInlineGetBodies_UnreadableDiskPathIsAnError(t *testing.T) {
	store := &fakeStore{getEntry: Entry{DiskPath: filepath.Join(t.TempDir(), "does-not-exist")}, getOK: true}
	h := InlineGetBodies(NewHandler(store))

	if err := h.Get(&protocol.Request{ActionID: []byte("a")}, &protocol.Response{}); err == nil {
		t.Fatal("Get() err = nil, want an error when DiskPath can't be read")
	}
}

func TestInlineGetBodies_PutIsUnaffected(t *testing.T) {
	store := &fakeStore{putDiskPath: "/cache/new"}
	h := InlineGetBodies(NewHandler(store))

	resp := &protocol.Response{}
	if err := h.Put(&protocol.Request{ActionID: []byte("a"), OutputID: []byte("b")}, resp); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if resp.DiskPath != "/cache/new" {
		t.Errorf("resp.DiskPath = %q, want /cache/new", resp.DiskPath)
	}
	if resp.Body != nil || resp.BodyInline || resp.BodySize != 0 {
		t.Errorf("resp.Body=%v resp.BodyInline=%v resp.BodySize=%d, want all zero for a put", resp.Body, resp.BodyInline, resp.BodySize)
	}
}
