package tiered_test

import (
	"bytes"
	"errors"
	"fmt"
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
	s.Flush() // the remote push happens in the background
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
	s.Flush()
	if gotPath != localPath {
		t.Errorf("Put returned %q, want the local path %q despite the remote failure", gotPath, localPath)
	}
	if !logged {
		t.Error("expected a Logf call noting the remote failure")
	}
}

// TestGet_RemoteHitWithUnopenableDiskPathIsAMiss is the regression test
// for handing the go command a path tiered had just failed to open (e.g.
// because the server evicted it between its Get and ours): the go command
// can't read it either, so it must be reported as a miss.
func TestGet_RemoteHitWithUnopenableDiskPathIsAMiss(t *testing.T) {
	local := &fakeStore{}
	remoteEntry := cas.Entry{OutputID: []byte("out"), Size: 3, DiskPath: filepath.Join(t.TempDir(), "does-not-exist")}
	remote := &fakeStore{getFunc: func([]byte) (cas.Entry, bool, error) { return remoteEntry, true, nil }}
	var logged bool
	s := &tiered.Store{Local: local, Remote: remote, Logf: func(string, ...any) { logged = true }}

	entry, ok, err := s.Get([]byte("a"))
	if err != nil {
		t.Fatalf("Get err = %v, want nil", err)
	}
	if ok {
		t.Errorf("Get = %+v ok=true, want a miss for a remote path that can't be opened", entry)
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
	s.Flush()
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

// fetchingStore is a fakeStore that also implements cas.Fetcher, the way
// remote.Store does for inline-body hits.
type fetchingStore struct {
	fakeStore
	fetchFunc func([]byte) (cas.Entry, io.Reader, bool, error)
}

func (f *fetchingStore) Fetch(actionID []byte) (cas.Entry, io.Reader, bool, error) {
	f.getCalls++
	return f.fetchFunc(actionID)
}

func TestGet_FetchedBodyWritesStraightToLocal(t *testing.T) {
	var localBody []byte
	local := &fakeStore{putFunc: func(_, _ []byte, body io.Reader, _ int64) (string, error) {
		localBody, _ = io.ReadAll(body)
		return "/local/path", nil
	}}
	remote := &fetchingStore{fetchFunc: func([]byte) (cas.Entry, io.Reader, bool, error) {
		return cas.Entry{OutputID: []byte("out"), Size: 5}, bytes.NewReader([]byte("bytes")), true, nil
	}}
	s := &tiered.Store{Local: local, Remote: remote}

	entry, ok, err := s.Get([]byte("a"))
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if entry.DiskPath != "/local/path" || string(localBody) != "bytes" {
		t.Errorf("entry.DiskPath=%q local body=%q, want the fetched bytes written to /local/path", entry.DiskPath, localBody)
	}
}

func TestGet_FetchedBodyLocalWriteFailureIsAMiss(t *testing.T) {
	local := &fakeStore{putFunc: func([]byte, []byte, io.Reader, int64) (string, error) { return "", errors.New("disk full") }}
	remote := &fetchingStore{fetchFunc: func([]byte) (cas.Entry, io.Reader, bool, error) {
		return cas.Entry{OutputID: []byte("out"), Size: 1}, bytes.NewReader([]byte("x")), true, nil
	}}
	s := &tiered.Store{Local: local, Remote: remote}

	if _, ok, err := s.Get([]byte("a")); ok || err != nil {
		t.Fatalf("Get = ok %v err %v, want a plain miss (there's no file to serve instead)", ok, err)
	}
}

// TestRemoteUnavailableIsSkippedAfterwards: once Remote reports
// cas.ErrUnavailable, neither Gets nor Puts should touch it again -- each
// would just fail the same way, paying a timeout and a log line apiece.
func TestRemoteUnavailableIsSkippedAfterwards(t *testing.T) {
	local := &fakeStore{}
	remote := &fakeStore{getFunc: func([]byte) (cas.Entry, bool, error) {
		return cas.Entry{}, false, fmt.Errorf("%w: connection reset", cas.ErrUnavailable)
	}}
	var logs int
	s := &tiered.Store{Local: local, Remote: remote, Logf: func(string, ...any) { logs++ }}

	for i := 0; i < 3; i++ {
		if _, ok, err := s.Get([]byte("a")); ok || err != nil {
			t.Fatalf("Get #%d = ok %v err %v, want a plain miss", i, ok, err)
		}
	}
	if _, err := s.Put([]byte("a"), []byte("out"), bytes.NewReader([]byte("x")), 1); err != nil {
		t.Fatalf("Put: %v", err)
	}
	s.Flush()
	if remote.getCalls != 1 || remote.putCalls != 0 {
		t.Errorf("remote saw %d gets / %d puts, want 1 / 0 (skipped once unavailable)", remote.getCalls, remote.putCalls)
	}
	if logs != 1 {
		t.Errorf("logged %d times, want once", logs)
	}
}

func TestPut_ReadOnlyRemoteStopsPushing(t *testing.T) {
	dir := t.TempDir()
	local := &fakeStore{putFunc: func(actionID, _ []byte, body io.Reader, _ int64) (string, error) {
		p := filepath.Join(dir, string(actionID))
		b, _ := io.ReadAll(body)
		return p, os.WriteFile(p, b, 0o644)
	}}
	remote := &fakeStore{putFunc: func([]byte, []byte, io.Reader, int64) (string, error) {
		return "", fmt.Errorf("remote put: %w", cas.ErrReadOnly)
	}}
	s := &tiered.Store{Local: local, Remote: remote}

	for _, id := range []string{"a", "b", "c"} {
		if _, err := s.Put([]byte(id), []byte("out"), bytes.NewReader([]byte("x")), 1); err != nil {
			t.Fatalf("Put(%s): %v", id, err)
		}
		s.Flush()
	}
	if remote.putCalls != 1 {
		t.Errorf("remote.Put called %d times, want 1 (pushing stops once the remote says it's read-only)", remote.putCalls)
	}
}

func TestClose_DrainsQueuedUploads(t *testing.T) {
	dir := t.TempDir()
	localPath := filepath.Join(dir, "body")
	local := &fakeStore{putFunc: func(_, _ []byte, body io.Reader, _ int64) (string, error) {
		b, _ := io.ReadAll(body)
		return localPath, os.WriteFile(localPath, b, 0o644)
	}}
	remote := &fakeStore{}
	s := &tiered.Store{Local: local, Remote: remote}

	if _, err := s.Put([]byte("a"), []byte("out"), bytes.NewReader([]byte("x")), 1); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if remote.putCalls != 1 {
		t.Errorf("remote.Put called %d times, want 1 (Close must finish queued uploads before closing the remote)", remote.putCalls)
	}
	// A Put after Close mustn't panic on the closed queue.
	if _, err := s.Put([]byte("b"), []byte("out"), bytes.NewReader([]byte("x")), 1); err != nil {
		t.Fatalf("Put after Close: %v", err)
	}
}
