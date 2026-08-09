// Package sockpath computes the default Unix socket path shared by
// cmd/cacheserver and cmd/cacheprog, so both agree on where the daemon
// lives without either side needing a flag pointed at the other.
package sockpath

import (
	"fmt"
	"os"
	"path/filepath"
)

// Default returns the default Unix socket path, namespaced by UID so
// multiple users on the same machine don't collide.
//
// Unix-only for now (os.Getuid has no meaningful equivalent on Windows,
// where this would need a named pipe instead) -- not solved speculatively
// here since there's no Windows build to validate it against yet.
func Default() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("buildcache-%d.sock", os.Getuid()))
}
