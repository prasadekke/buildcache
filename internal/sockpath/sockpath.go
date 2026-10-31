// Package sockpath computes the default Unix socket path shared by
// cmd/cacheserver and cmd/cacheprog, so both agree on where the daemon
// lives without either side needing a flag pointed at the other, and
// guards that path against other local users.
//
// The guarding matters because a same-machine cacheserver is trusted
// completely: cacheprog hands the DiskPaths it returns straight to the go
// command, which links whatever it finds there into the build. On a
// multi-user machine with a shared /tmp, a predictable socket path that
// anyone can create first would let another user stand up a fake daemon
// and feed arbitrary objects into your builds. So the default socket lives
// in a per-user directory only its owner can write to, cacheserver refuses
// to use one owned by anyone else, and cacheprog refuses to talk to a
// socket it doesn't own.
//
// Ownership checks are Unix-only (see owner_unix.go); elsewhere they're
// skipped, which matches the Unix socket itself being Unix-only for now.
package sockpath

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Default returns the default Unix socket path: a socket inside a
// directory under the OS temp dir that's namespaced by UID, so multiple
// users on the same machine neither collide nor can reach each other's
// daemon. See PrepareDir.
func Default() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("buildcache-%d", os.Getuid()), "cacheserver.sock")
}

// PrepareDir creates path's parent directory if needed, private to the
// current user (0700), and verifies that an existing one is a real
// directory owned by the current user -- tightening its permissions if
// they're looser than 0700. cacheserver calls it for the default socket
// path; a path chosen explicitly with -socket is the caller's to arrange.
func PrepareDir(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating socket directory %s: %w", dir, err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("socket directory %s is not a directory", dir)
	}
	if err := checkOwnedByMe(dir, fi); err != nil {
		return err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("restricting socket directory %s to its owner: %w", dir, err)
		}
	}
	return nil
}

// CheckOwner verifies that path is a Unix socket owned by the current
// user, which cacheprog requires before trusting anything a daemon there
// says. If nothing exists at path, the error satisfies
// errors.Is(err, fs.ErrNotExist), so callers can tell "no daemon" (normal)
// apart from "someone else's socket" (worth a warning).
func CheckOwner(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s is not a Unix socket", path)
	}
	return checkOwnedByMe(path, fi)
}

// RemoveStale removes a socket left at path by an earlier cacheserver that
// didn't shut down cleanly. It refuses to remove anything that isn't a
// socket owned by the current user, rather than deleting whatever happens
// to be at a mistyped -socket path.
func RemoveStale(path string) error {
	err := CheckOwner(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("refusing to replace %s: %w", path, err)
	}
	return os.Remove(path)
}

func checkOwnedByMe(path string, fi os.FileInfo) error {
	uid, ok := ownerUID(fi)
	if !ok {
		return nil // no ownership information on this platform
	}
	if me := os.Getuid(); uid != me {
		return fmt.Errorf("%s is owned by uid %d, not the current user (uid %d)", path, uid, me)
	}
	return nil
}
