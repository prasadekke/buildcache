package sockpath

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestDefault_IncludesUID(t *testing.T) {
	got := Default()
	want := strconv.Itoa(os.Getuid())
	if !strings.Contains(got, want) {
		t.Errorf("Default() = %q, want it to contain uid %q", got, want)
	}
}

func TestDefault_Deterministic(t *testing.T) {
	a := Default()
	b := Default()
	if a != b {
		t.Errorf("Default() returned different paths on two calls: %q vs %q", a, b)
	}
}

func TestDefault_UnderTempDir(t *testing.T) {
	got := Default()
	if !strings.HasPrefix(got, os.TempDir()) {
		t.Errorf("Default() = %q, want a path under %q", got, os.TempDir())
	}
}

// shortTempDir returns a temp dir with a short enough path for a Unix
// socket inside it (sun_path is ~104 bytes on macOS; t.TempDir's paths
// can exceed that).
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sp")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestPrepareDir_CreatesPrivateDir(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "sub", "s.sock")
	if err := PrepareDir(path); err != nil {
		t.Fatalf("PrepareDir: %v", err)
	}
	fi, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("socket dir perm = %o, want 700", perm)
	}
}

func TestPrepareDir_TightensLoosePermissions(t *testing.T) {
	dir := filepath.Join(shortTempDir(t), "loose")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	os.Chmod(dir, 0o777) // defeat umask
	if err := PrepareDir(filepath.Join(dir, "s.sock")); err != nil {
		t.Fatalf("PrepareDir: %v", err)
	}
	fi, _ := os.Stat(dir)
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("socket dir perm = %o after PrepareDir, want 700", perm)
	}
}

func TestPrepareDir_RejectsSymlinkedDir(t *testing.T) {
	base := shortTempDir(t)
	target := filepath.Join(base, "real")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := PrepareDir(filepath.Join(link, "s.sock")); err == nil {
		t.Fatal("PrepareDir err = nil, want a refusal for a symlinked socket directory")
	}
}

func TestCheckOwner(t *testing.T) {
	dir := shortTempDir(t)

	missing := filepath.Join(dir, "missing.sock")
	if err := CheckOwner(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("CheckOwner(missing) = %v, want fs.ErrNotExist", err)
	}

	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := CheckOwner(regular); err == nil {
		t.Error("CheckOwner(regular file) = nil, want an error")
	}
	if err := RemoveStale(regular); err == nil {
		t.Error("RemoveStale(regular file) = nil, want a refusal")
	}
	if _, err := os.Stat(regular); err != nil {
		t.Error("RemoveStale deleted a regular file it should have refused to touch")
	}

	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	if err := CheckOwner(sock); err != nil {
		t.Errorf("CheckOwner(own socket) = %v, want nil", err)
	}
	if err := RemoveStale(sock); err != nil {
		t.Errorf("RemoveStale(own socket) = %v, want nil", err)
	}
	if _, err := os.Lstat(sock); !os.IsNotExist(err) {
		t.Error("RemoveStale left its own stale socket in place")
	}
}
