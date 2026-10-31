// Package cachedir holds the default cache directory shared by
// cmd/cacheprog and cmd/cacheserver.
//
// Both default to the same directory on purpose: on one machine, for one
// user, there's only ever one useful local store. A cacheserver started
// with the default -dir and a cacheprog using the default -dir therefore
// share a store; cacheprog notices (via the daemon's advertised StoreDir)
// and uses the directory directly instead of layering it on top of itself.
package cachedir

import (
	"os"
	"path/filepath"
)

// Default returns the default cache directory: "buildcache" under the
// user's cache directory, or ".buildcache" in the working directory if
// there isn't one.
func Default() string {
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "buildcache")
	}
	return ".buildcache"
}
