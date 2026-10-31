package disk

import (
	"bytes"
	"encoding/json"
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
	bodyPath, err := s.Put(actionID, []byte("out"), bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
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
	bodyPath, err := s.Put(actionID, []byte("out"), bytes.NewReader([]byte("x")), 1)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

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
		bodyPath, err := s.Put(actionID, []byte("out-"+name), bytes.NewReader(body), sizes)
		if err != nil {
			t.Fatalf("Put(%s): %v", name, err)
		}
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

	// An orphaned body (no meta refers to it) should be swept
	// unconditionally once it's older than the grace period, even with a
	// cap large enough that nothing else needs evicting.
	orphanBodyPath := s.bodyPath([]byte("orphan-body"), []byte("out"))
	if err := os.MkdirAll(filepath.Dir(orphanBodyPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	orphanBytes := []byte("orphan bytes")
	if err := os.WriteFile(orphanBodyPath, orphanBytes, 0o644); err != nil {
		t.Fatalf("writing orphan body: %v", err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(orphanBodyPath, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
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
	metaPath := s.metaPath(actionID)
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
	bodyPath, metaPath := s.legacyBodyPath(actionID), s.metaPath(actionID)
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
	bodyPath, metaPath := s.legacyBodyPath(actionID), s.metaPath(actionID)
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
	bodyPath := s.bodyPath(actionID, []byte("out"))
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

// TestEvict_SparesFreshOrphanBody is the regression test for the Put/Evict
// race: a sweep running between a Put's body write and its meta write
// sees a body no meta refers to yet. It must leave that body alone, or the
// Put returns a DiskPath that no longer exists.
func TestEvict_SparesFreshOrphanBody(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	// Exactly the on-disk state mid-Put: body renamed into place, no meta.
	bodyPath := s.bodyPath([]byte("in-flight"), []byte("out"))
	if err := os.MkdirAll(filepath.Dir(bodyPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(bodyPath, []byte("still being put"), 0o644); err != nil {
		t.Fatalf("writing body: %v", err)
	}

	if _, err := s.Evict(0); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if _, err := os.Stat(bodyPath); err != nil {
		t.Fatalf("in-flight body was removed by Evict: %v", err)
	}
}

func TestEvict_SparesRecentlyUsedEvenOverCap(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	if _, err := s.Put([]byte("hot"), []byte("out"), bytes.NewReader([]byte("0123456789")), 10); err != nil {
		t.Fatalf("Put: %v", err)
	}
	freed, err := s.Evict(0)
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if freed != 0 {
		t.Errorf("freed = %d, want 0 (the only entry was just used)", freed)
	}
	if _, ok, _ := s.Get([]byte("hot")); !ok {
		t.Error("recently used entry was evicted despite the grace period")
	}

	s.EvictGrace = 0
	if freed, err := s.Evict(0); err != nil || freed != 10 {
		t.Errorf("Evict with no grace: freed=%d err=%v, want 10 bytes freed", freed, err)
	}
}

func TestEvict_RemovesStaleTempAndOrphanMeta(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	bodyPath, err := s.Put([]byte("gone"), []byte("out"), bytes.NewReader([]byte("x")), 1)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	os.Remove(bodyPath)
	metaPath := s.metaPath([]byte("gone"))

	tmpPath := filepath.Join(filepath.Dir(metaPath), tmpPrefix+"abandoned")
	if err := os.WriteFile(tmpPath, []byte("half a put"), 0o644); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}
	old := time.Now().Add(-time.Hour)
	for _, p := range []string{metaPath, tmpPath} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatalf("Chtimes(%s): %v", p, err)
		}
	}

	if _, err := s.Evict(1 << 20); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	for _, p := range []string{metaPath, tmpPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists after Evict, want it swept as debris", p)
		}
	}
}

// TestGet_MetaAlwaysMatchesItsBody covers the interleaving that a single
// per-action body file allowed: two Puts of one action with different
// outputs, whose meta writes land in the opposite order from their body
// writes. Whichever meta wins, Get must return that meta's own bytes.
func TestGet_MetaAlwaysMatchesItsBody(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	actionID := []byte("nondeterministic")
	if _, err := s.Put(actionID, []byte("out-a"), bytes.NewReader([]byte("AAAA")), 4); err != nil {
		t.Fatalf("Put A: %v", err)
	}
	metaA, err := os.ReadFile(s.metaPath(actionID))
	if err != nil {
		t.Fatalf("reading meta A: %v", err)
	}
	if _, err := s.Put(actionID, []byte("out-b"), bytes.NewReader([]byte("BBBBBB")), 6); err != nil {
		t.Fatalf("Put B: %v", err)
	}
	// Simulate A's meta write landing last.
	if err := os.WriteFile(s.metaPath(actionID), metaA, 0o644); err != nil {
		t.Fatalf("restoring meta A: %v", err)
	}

	entry, ok, err := s.Get(actionID)
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	got, err := os.ReadFile(entry.DiskPath)
	if err != nil {
		t.Fatalf("reading DiskPath: %v", err)
	}
	if string(entry.OutputID) != "out-a" || string(got) != "AAAA" || entry.Size != 4 {
		t.Errorf("Get = OutputID %q, body %q, size %d; want out-a's own body AAAA (4 bytes)", entry.OutputID, got, entry.Size)
	}
}

func TestGet_LegacyLayoutStillReadable(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	actionID := []byte("from-before")
	bodyPath := s.legacyBodyPath(actionID)
	if err := os.MkdirAll(filepath.Dir(bodyPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(bodyPath, []byte("old bytes"), 0o644); err != nil {
		t.Fatalf("writing legacy body: %v", err)
	}
	mb, _ := json.Marshal(map[string]any{"OutputID": "6f6c64", "Time": time.Now().UTC()})
	if err := os.WriteFile(s.metaPath(actionID), mb, 0o644); err != nil {
		t.Fatalf("writing legacy meta: %v", err)
	}

	entry, ok, err := s.Get(actionID)
	if err != nil || !ok {
		t.Fatalf("Get legacy entry: ok=%v err=%v", ok, err)
	}
	if entry.DiskPath != bodyPath || string(entry.OutputID) != "old" {
		t.Errorf("entry = %+v, want the legacy body at %s with OutputID \"old\"", entry, bodyPath)
	}
}

func TestGet_RejectsMetaNamingBodyOutsideShard(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	actionID := []byte("escape")
	metaPath := s.metaPath(actionID)
	if err := os.MkdirAll(filepath.Dir(metaPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mb, _ := json.Marshal(meta{OutputID: "00", Time: time.Now(), Body: "../../etc/passwd.body"})
	if err := os.WriteFile(metaPath, mb, 0o644); err != nil {
		t.Fatalf("writing meta: %v", err)
	}
	if _, _, err := s.Get(actionID); err == nil {
		t.Fatal("Get err = nil, want an error for a meta whose Body escapes the shard directory")
	}
}
