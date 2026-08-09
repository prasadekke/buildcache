package tiered_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/prasadekke/buildcache/internal/cas"
	"github.com/prasadekke/buildcache/internal/cas/tiered"
)

// fakeStore is a cas.Store test double that records call counts and lets
// each test control behavior via optional funcs.
type fakeStore struct {
	getFunc  func([]byte) (cas.Entry, bool, error)
	putFunc  func(actionID, outputID []byte, body io.Reader, size int64) (string, error)
	closeErr error

	getCalls int
	putCalls int
	closed   bool
}

func (f *fakeStore) Get(actionID []byte) (cas.Entry, bool, error) {
	f.getCalls++
	if f.getFunc != nil {
		return f.getFunc(actionID)
	}
	return cas.Entry{}, false, nil
}

func (f *fakeStore) Put(actionID, outputID []byte, body io.Reader, size int64) (string, error) {
	f.putCalls++
	if f.putFunc != nil {
		return f.putFunc(actionID, outputID, body, size)
	}
	return "/fake/path", nil
}

func (f *fakeStore) Close() error {
	f.closed = true
	return f.closeErr
}

func TestGet_LocalHitSkipsRemote(t *testing.T) {
	wantEntry := cas.Entry{OutputID: []byte("out"), Size: 5, DiskPath: "/local/path"}
	local := &fakeStore{getFunc: func([]byte) (cas.Entry, bool, error) { return wantEntry, true, nil }}
	remote := &fakeStore{}
	s := &tiered.Store{Local: local, Remote: remote}

	entry, ok, err := s.Get([]byte("a"))
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if entry.DiskPath != wantEntry.DiskPath {
		t.Errorf("entry = %+v, want %+v", entry, wantEntry)
	}
	if remote.getCalls != 0 {
		t.Errorf("remote.Get called %d times, want 0 (local hit should skip remote)", remote.getCalls)
	}
}

func TestGet_LocalMissRemoteHitWritesThrough(t *testing.T) {
	dir := t.TempDir()
	remotePath := filepath.Join(dir, "remote-body")
	if err := os.WriteFile(remotePath, []byte("remote bytes"), 0o644); err != nil {
		t.Fatalf("writing remote body: %v", err)
	}
	localPath := filepath.Join(dir, "local-body")

	local := &fakeStore{
		putFunc: func(actionID, outputID []byte, body io.Reader, size int64) (string, error) {
			b, err := io.ReadAll(body)
			if err != nil {
				return "", err
			}
			if err := os.WriteFile(localPath, b, 0o644); err != nil {
				return "", err
			}
			return localPath, nil
		},
	}
	remote := &fakeStore{
		getFunc: func([]byte) (cas.Entry, bool, error) {
			return cas.Entry{OutputID: []byte("out"), Size: int64(len("remote bytes")), DiskPath: remotePath}, true, nil
		},
	}
	s := &tiered.Store{Local: local, Remote: remote}

	entry, ok, err := s.Get([]byte("a"))
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if local.putCalls != 1 {
		t.Errorf("local.Put called %d times, want 1 (a remote hit should be written through)", local.putCalls)
	}
	if entry.DiskPath != localPath {
		t.Errorf("entry.DiskPath = %q, want the local write-through path %q", entry.DiskPath, localPath)
	}
}

func TestGet_BothMiss(t *testing.T) {
	local := &fakeStore{}
	remote := &fakeStore{}
	s := &tiered.Store{Local: local, Remote: remote}

	_, ok, err := s.Get([]byte("a"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Fatal("Get: ok = true, want false")
	}
}

func TestGet_RemoteErrorDegradesToMiss(t *testing.T) {
	local := &fakeStore{}
	remote := &fakeStore{
		getFunc: func([]byte) (cas.Entry, bool, error) { return cas.Entry{}, false, errors.New("remote unreachable") },
	}
	var logged bool
	s := &tiered.Store{Local: local, Remote: remote, Logf: func(string, ...any) { logged = true }}

	_, ok, err := s.Get([]byte("a"))
	if err != nil {
		t.Fatalf("Get err = %v, want nil (a remote error should degrade to a plain miss)", err)
	}
	if ok {
		t.Fatal("Get: ok = true, want false")
	}
	if !logged {
		t.Error("expected a Logf call noting the remote failure")
	}
}

func TestGet_LocalErrorPropagates(t *testing.T) {
	wantErr := errors.New("disk exploded")
	local := &fakeStore{getFunc: func([]byte) (cas.Entry, bool, error) { return cas.Entry{}, false, wantErr }}
	remote := &fakeStore{}
	s := &tiered.Store{Local: local, Remote: remote}

	_, _, err := s.Get([]byte("a"))
	if !errors.Is(err, wantErr) {
		t.Fatalf("Get err = %v, want %v", err, wantErr)
	}
	if remote.getCalls != 0 {
		t.Errorf("remote.Get called %d times, want 0 (a local error shouldn't fall through to remote)", remote.getCalls)
	}
}

func TestPut_WritesLocalAndRemote(t *testing.T) {
	dir := t.TempDir()
	localPath := filepath.Join(dir, "local-body")
	local := &fakeStore{
		putFunc: func(actionID, outputID []byte, body io.Reader, size int64) (string, error) {
			b, err := io.ReadAll(body)
			if err != nil {
				return "", err
			}
			if err := os.WriteFile(localPath, b, 0o644); err != nil {
				return "", err
			}
			return localPath, nil
		},
	}
	var remoteBody []byte
	remote := &fakeStore{
		putFunc: func(actionID, outputID []byte, body io.Reader, size int64) (string, error) {
			b, err := io.ReadAll(body)
			if err != nil {
				return "", err
			}
			remoteBody = b
			return "/remote/path", nil
		},
	}
	s := &tiered.Store{Local: local, Remote: remote}

	body := []byte("push me upstream")
	gotPath, err := s.Put([]byte("a"), []byte("out"), bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if gotPath != localPath {
		t.Errorf("Put returned %q, want the local path %q", gotPath, localPath)
	}
	if remote.putCalls != 1 {
		t.Errorf("remote.Put called %d times, want 1", remote.putCalls)
	}
	if string(remoteBody) != string(body) {
		t.Errorf("remote received body %q, want %q", remoteBody, body)
	}
}

func TestPut_LocalFailurePropagates(t *testing.T) {
	wantErr := errors.New("disk full")
	local := &fakeStore{putFunc: func([]byte, []byte, io.Reader, int64) (string, error) { return "", wantErr }}
	remote := &fakeStore{}
	s := &tiered.Store{Local: local, Remote: remote}

	_, err := s.Put([]byte("a"), []byte("out"), bytes.NewReader([]byte("x")), 1)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Put err = %v, want %v", err, wantErr)
	}
	if remote.putCalls != 0 {
		t.Errorf("remote.Put called %d times, want 0 (a local failure shouldn't attempt a remote push)", remote.putCalls)
	}
}

func TestPut_RemoteFailureIsNonFatal(t *testing.T) {
	dir := t.TempDir()
	localPath := filepath.Join(dir, "local-body")
	local := &fakeStore{
		putFunc: func(actionID, outputID []byte, body io.Reader, size int64) (string, error) {
			b, err := io.ReadAll(body)
			if err != nil {
				return "", err
			}
			if err := os.WriteFile(localPath, b, 0o644); err != nil {
				return "", err
			}
			return localPath, nil
		},
	}
	remote := &fakeStore{putFunc: func([]byte, []byte, io.Reader, int64) (string, error) { return "", errors.New("remote down") }}
	var logged bool
	s := &tiered.Store{Local: local, Remote: remote, Logf: func(string, ...any) { logged = true }}

	gotPath, err := s.Put([]byte("a"), []byte("out"), bytes.NewReader([]byte("x")), 1)
	if err != nil {
		t.Fatalf("Put err = %v, want nil (a remote failure shouldn't fail the Put)", err)
	}
	if gotPath != localPath {
		t.Errorf("Put returned %q, want the local path %q despite the remote failure", gotPath, localPath)
	}
	if !logged {
		t.Error("expected a Logf call noting the remote failure")
	}
}

func TestGet_RemoteHitWithUnopenableDiskPathServesFromRemote(t *testing.T) {
	local := &fakeStore{}
	remoteEntry := cas.Entry{OutputID: []byte("out"), Size: 3, DiskPath: filepath.Join(t.TempDir(), "does-not-exist")}
	remote := &fakeStore{getFunc: func([]byte) (cas.Entry, bool, error) { return remoteEntry, true, nil }}
	var logged bool
	s := &tiered.Store{Local: local, Remote: remote, Logf: func(string, ...any) { logged = true }}

	entry, ok, err := s.Get([]byte("a"))
	if err != nil {
		t.Fatalf("Get err = %v, want nil (an unopenable remote hit should still be served from remote)", err)
	}
	if !ok || entry.DiskPath != remoteEntry.DiskPath {
		t.Errorf("entry = %+v ok=%v, want the remote entry served as-is", entry, ok)
	}
	if local.putCalls != 0 {
		t.Errorf("local.Put called %d times, want 0 (nothing to write through when the remote file can't be opened)", local.putCalls)
	}
	if !logged {
		t.Error("expected a Logf call noting the open failure")
	}
}

func TestGet_LocalWriteThroughFailureServesFromRemote(t *testing.T) {
	dir := t.TempDir()
	remotePath := filepath.Join(dir, "remote-body")
	if err := os.WriteFile(remotePath, []byte("x"), 0o644); err != nil {
		t.Fatalf("writing remote body: %v", err)
	}
	remoteEntry := cas.Entry{OutputID: []byte("out"), Size: 1, DiskPath: remotePath}

	wantErr := errors.New("local disk full")
	local := &fakeStore{putFunc: func([]byte, []byte, io.Reader, int64) (string, error) { return "", wantErr }}
	remote := &fakeStore{getFunc: func([]byte) (cas.Entry, bool, error) { return remoteEntry, true, nil }}
	var logged bool
	s := &tiered.Store{Local: local, Remote: remote, Logf: func(string, ...any) { logged = true }}

	entry, ok, err := s.Get([]byte("a"))
	if err != nil {
		t.Fatalf("Get err = %v, want nil (a local write-through failure should still serve from remote)", err)
	}
	if !ok || entry.DiskPath != remotePath {
		t.Errorf("entry = %+v ok=%v, want the remote entry served as-is", entry, ok)
	}
	if !logged {
		t.Error("expected a Logf call noting the write-through failure")
	}
}

func TestPut_UnopenableLocalPathSkipsRemotePush(t *testing.T) {
	local := &fakeStore{putFunc: func([]byte, []byte, io.Reader, int64) (string, error) {
		return "/does/not/exist", nil // Put "succeeds" but the path isn't real
	}}
	remote := &fakeStore{}
	var logged bool
	s := &tiered.Store{Local: local, Remote: remote, Logf: func(string, ...any) { logged = true }}

	gotPath, err := s.Put([]byte("a"), []byte("out"), bytes.NewReader([]byte("x")), 1)
	if err != nil {
		t.Fatalf("Put err = %v, want nil (an unopenable local path shouldn't fail the Put itself)", err)
	}
	if gotPath != "/does/not/exist" {
		t.Errorf("Put returned %q, want the local path returned even though it couldn't be reopened", gotPath)
	}
	if remote.putCalls != 0 {
		t.Errorf("remote.Put called %d times, want 0 (nothing to push if the local file can't be reopened)", remote.putCalls)
	}
	if !logged {
		t.Error("expected a Logf call noting the reopen failure")
	}
}

func TestClose_ClosesBothTiers(t *testing.T) {
	local := &fakeStore{}
	remote := &fakeStore{}
	s := &tiered.Store{Local: local, Remote: remote}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !local.closed || !remote.closed {
		t.Errorf("local.closed=%v remote.closed=%v, want both true", local.closed, remote.closed)
	}
}

func TestClose_RemoteErrorStillClosesLocal(t *testing.T) {
	local := &fakeStore{}
	remote := &fakeStore{closeErr: errors.New("remote close failed")}
	s := &tiered.Store{Local: local, Remote: remote}

	if err := s.Close(); err == nil {
		t.Fatal("Close() err = nil, want the remote close error surfaced")
	}
	if !local.closed {
		t.Error("local was not closed despite the remote failing to close")
	}
}
