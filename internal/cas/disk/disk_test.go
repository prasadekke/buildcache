package disk

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNew_CreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "cache")
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("New did not create %s", dir)
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	actionID := []byte("action-id-bytes")
	outputID := []byte("output-id-bytes")
	body := []byte("cached object contents")

	diskPath, err := s.Put(actionID, outputID, bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if diskPath == "" {
		t.Fatal("Put returned empty diskPath")
	}

	entry, ok, err := s.Get(actionID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("Get: ok = false, want true after Put")
	}
	if string(entry.OutputID) != string(outputID) {
		t.Errorf("entry.OutputID = %q, want %q", entry.OutputID, outputID)
	}
	if entry.Size != int64(len(body)) {
		t.Errorf("entry.Size = %d, want %d", entry.Size, len(body))
	}
	if entry.DiskPath != diskPath {
		t.Errorf("entry.DiskPath = %q, want %q", entry.DiskPath, diskPath)
	}
	got, err := os.ReadFile(entry.DiskPath)
	if err != nil {
		t.Fatalf("reading DiskPath: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("body on disk = %q, want %q", got, body)
	}
}

func TestGet_Miss(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	_, ok, err := s.Get([]byte("never-stored"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Fatal("Get: ok = true, want false for a never-stored action")
	}
}

// TestPut_ZeroByteBody is the storage-layer companion to the protocol-layer
// regression test: a zero-byte body must be storable via a valid empty
// io.Reader.
func TestPut_ZeroByteBody(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	actionID := []byte("empty-action")
	diskPath, err := s.Put(actionID, []byte("empty-output"), bytes.NewReader(nil), 0)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	entry, ok, err := s.Get(actionID)
	if err != nil || !ok {
		t.Fatalf("Get after zero-byte Put: ok=%v err=%v", ok, err)
	}
	if entry.Size != 0 {
		t.Errorf("entry.Size = %d, want 0", entry.Size)
	}
	if entry.DiskPath != diskPath {
		t.Errorf("entry.DiskPath = %q, want %q", entry.DiskPath, diskPath)
	}
}

// TestPut_NilBodyDoesNotPanic guards the defensive nil check in Put: a nil
// io.Reader should never reach Store.Put in practice, but Put must not
// crash the whole process if it does -- a panic here inside cacheserver
// would take down every other concurrent connection, not just the one bad
// request.
func TestPut_NilBodyDoesNotPanic(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Put panicked on a nil body: %v", r)
		}
	}()
	if _, err := s.Put([]byte("a"), []byte("b"), nil, 0); err != nil {
		t.Fatalf("Put with nil body: %v", err)
	}
}

func TestGet_OrphanedMetaWithoutBody(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	actionID := []byte("orphan-action")
	body := []byte("x")
	if _, err := s.Put(actionID, []byte("out"), bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	bodyPath, _ := s.paths(actionID)
	if err := os.Remove(bodyPath); err != nil {
		t.Fatalf("removing body to simulate an orphaned meta: %v", err)
	}

	_, ok, err := s.Get(actionID)
	if err != nil {
		t.Fatalf("Get: %v, want nil (an orphaned meta is a miss, not an error)", err)
	}
	if ok {
		t.Fatal("Get: ok = true, want false for an orphaned meta with no body")
	}
}

func TestGet_BumpsMtimeOnHit(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	actionID := []byte("mtime-action")
	if _, err := s.Put(actionID, []byte("out"), bytes.NewReader([]byte("x")), 1); err != nil {
		t.Fatalf("Put: %v", err)
	}
	bodyPath, _ := s.paths(actionID)

	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(bodyPath, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	if _, _, err := s.Get(actionID); err != nil {
		t.Fatalf("Get: %v", err)
	}

	fi, err := os.Stat(bodyPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !fi.ModTime().After(old) {
		t.Errorf("mtime after Get = %v, want it bumped past %v", fi.ModTime(), old)
	}
}

func TestEvict_KeepsMostRecentlyUsed(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	// Three 10-byte entries, staggered mtimes via explicit Chtimes since
	// mtime resolution on some filesystems is coarser than this test could
	// otherwise separate reliably.
	entries := []string{"oldest", "middle", "newest"}
	const sizes = int64(10)
	body := bytes.Repeat([]byte("x"), int(sizes))
	base := time.Now().Add(-time.Hour)
	for i, name := range entries {
		actionID := []byte(name)
		if _, err := s.Put(actionID, []byte("out-"+name), bytes.NewReader(body), sizes); err != nil {
			t.Fatalf("Put(%s): %v", name, err)
		}
		bodyPath, _ := s.paths(actionID)
		mt := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(bodyPath, mt, mt); err != nil {
			t.Fatalf("Chtimes(%s): %v", name, err)
		}
	}

	// Cap at 2 entries' worth of bytes: only "oldest" should be evicted.
	freed, err := s.Evict(2 * sizes)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if freed != sizes {
		t.Errorf("freed = %d, want %d", freed, sizes)
	}

	if _, ok, _ := s.Get([]byte("oldest")); ok {
		t.Error("oldest entry survived Evict, want it removed")
	}
	if _, ok, _ := s.Get([]byte("middle")); !ok {
		t.Error("middle entry was evicted, want it kept")
	}
	if _, ok, _ := s.Get([]byte("newest")); !ok {
		t.Error("newest entry was evicted, want it kept")
	}
}

func TestEvict_NoOpUnderCap(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	if _, err := s.Put([]byte("a"), []byte("out"), bytes.NewReader([]byte("x")), 1); err != nil {
		t.Fatalf("Put: %v", err)
	}
	freed, err := s.Evict(1 << 20)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if freed != 0 {
		t.Errorf("freed = %d, want 0 (already under cap)", freed)
	}
	if _, ok, _ := s.Get([]byte("a")); !ok {
		t.Error("entry was evicted despite being under the cap")
	}
}

func TestEvict_RemovesOrphansRegardlessOfCap(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	if _, err := s.Put([]byte("kept"), []byte("out"), bytes.NewReader([]byte("x")), 1); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// An orphaned body (meta removed) should be swept unconditionally, even
	// with a cap large enough that nothing else needs evicting.
	orphanBodyPath, _ := s.paths([]byte("orphan-body"))
	if err := os.MkdirAll(filepath.Dir(orphanBodyPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	orphanBytes := []byte("orphan bytes")
	if err := os.WriteFile(orphanBodyPath, orphanBytes, 0o644); err != nil {
		t.Fatalf("writing orphan body: %v", err)
	}

	freed, err := s.Evict(1 << 20)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if freed != int64(len(orphanBytes)) {
		t.Errorf("freed = %d, want %d", freed, len(orphanBytes))
	}
	if _, err := os.Stat(orphanBodyPath); !os.IsNotExist(err) {
		t.Error("orphaned body file still exists after Evict")
	}
	if _, ok, _ := s.Get([]byte("kept")); !ok {
		t.Error("unrelated kept entry was removed by orphan cleanup")
	}
}

func TestWriteAtomic_NoLeftoverTempFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "out.txt")
	if err := writeAtomic(target, bytes.NewReader([]byte("data"))); err != nil {
		t.Fatalf("writeAtomic: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "out.txt" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir contents = %v, want just [out.txt] (no leftover temp file)", names)
	}
}

func TestGet_MetaReadError(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	actionID := []byte("dir-instead-of-meta")
	_, metaPath := s.paths(actionID)
	if err := os.MkdirAll(metaPath, 0o755); err != nil {
		t.Fatalf("mkdir at meta path: %v", err)
	}

	if _, _, err := s.Get(actionID); err == nil {
		t.Fatal("Get() err = nil, want an error when the meta path is a directory")
	}
}

func TestGet_CorruptedMetaJSON(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	actionID := []byte("corrupt-meta")
	bodyPath, metaPath := s.paths(actionID)
	if err := os.MkdirAll(filepath.Dir(bodyPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(bodyPath, []byte("body"), 0o644); err != nil {
		t.Fatalf("writing body: %v", err)
	}
	if err := os.WriteFile(metaPath, []byte("not json"), 0o644); err != nil {
		t.Fatalf("writing corrupt meta: %v", err)
	}

	if _, _, err := s.Get(actionID); err == nil {
		t.Fatal("Get() err = nil, want an error for corrupted meta JSON")
	}
}

func TestGet_CorruptedOutputIDHex(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	actionID := []byte("bad-output-id")
	bodyPath, metaPath := s.paths(actionID)
	if err := os.MkdirAll(filepath.Dir(bodyPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(bodyPath, []byte("body"), 0o644); err != nil {
		t.Fatalf("writing body: %v", err)
	}
	badMeta := `{"OutputID":"not-valid-hex!!","Time":"2024-01-01T00:00:00Z"}`
	if err := os.WriteFile(metaPath, []byte(badMeta), 0o644); err != nil {
		t.Fatalf("writing meta: %v", err)
	}

	if _, _, err := s.Get(actionID); err == nil {
		t.Fatal("Get() err = nil, want an error for a non-hex OutputID in meta")
	}
}

func TestPut_WriteBodyFailure(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	actionID := []byte("body-path-is-a-dir")
	bodyPath, _ := s.paths(actionID)
	if err := os.MkdirAll(bodyPath, 0o755); err != nil {
		t.Fatalf("mkdir at body path: %v", err)
	}

	if _, err := s.Put(actionID, []byte("out"), bytes.NewReader([]byte("x")), 1); err == nil {
		t.Fatal("Put() err = nil, want an error when the body path is already a directory")
	}
}

func TestWriteAtomic_CreateTempFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root, which bypasses the permission check this test relies on")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	err := writeAtomic(filepath.Join(dir, "out.txt"), bytes.NewReader([]byte("x")))
	if err == nil {
		t.Fatal("writeAtomic() err = nil, want an error when the directory isn't writable")
	}
}

func TestNew_MkdirAllFailure(t *testing.T) {
	base := t.TempDir()
	blocker := filepath.Join(base, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("writing blocker file: %v", err)
	}

	if _, err := New(filepath.Join(blocker, "cache")); err == nil {
		t.Fatal("New() err = nil, want an error when a path component is a regular file")
	}
}

func TestPut_MkdirAllFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root, which bypasses the permission check this test relies on")
	}

	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	if _, err := s.Put([]byte("never-written"), []byte("out"), bytes.NewReader([]byte("x")), 1); err == nil {
		t.Fatal("Put() err = nil, want an error when the shard directory can't be created")
	}
}

func TestPutGet_ConcurrentSafety(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	const n = 20
	done := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			actionID := []byte{byte(i)}
			body := bytes.Repeat([]byte{byte(i)}, 100)
			_, err := s.Put(actionID, []byte{byte(i)}, bytes.NewReader(body), int64(len(body)))
			done <- err
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-done; err != nil {
			t.Errorf("concurrent Put: %v", err)
		}
	}
	for i := 0; i < n; i++ {
		entry, ok, err := s.Get([]byte{byte(i)})
		if err != nil || !ok {
			t.Errorf("Get(%d): ok=%v err=%v", i, ok, err)
			continue
		}
		if entry.Size != 100 {
			t.Errorf("Get(%d).Size = %d, want 100", i, entry.Size)
		}
	}
}
