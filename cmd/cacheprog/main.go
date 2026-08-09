// Command cacheprog is the GOCACHEPROG subprocess the go command invokes
// directly. Point the go command at it with:
//
//	GOCACHEPROG="/path/to/cacheprog" go build ./...
//
// Requires the *invoking* go command to be 1.24+ (that's what defines
// GOCACHEPROG); this binary itself builds with any reasonably modern Go
// toolchain, since it only depends on the standard library.
//
// Two modes, tried in order:
//
//  1. Server mode: if a cacheserver daemon is listening on the default (or
//     -socket) Unix socket, cacheprog dials it and simply pipes its own
//     stdin/stdout to that connection. cacheserver speaks the exact same
//     wire protocol, so no re-framing happens here -- it's a byte-for-byte
//     relay in each direction, giving a warm, shared store across many
//     invocations instead of one disk.Store per subprocess.
//  2. Direct mode: if no daemon is reachable (or -no-server is set),
//     cacheprog opens its own local disk.Store and serves the protocol
//     itself. This keeps cacheprog usable with zero setup even if nobody
//     ever starts cacheserver -- "almost zero config" shouldn't regress to
//     "now you must also run a daemon."
package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/prasad-ekke/buildcache/internal/cas"
	"github.com/prasad-ekke/buildcache/internal/cas/disk"
	"github.com/prasad-ekke/buildcache/internal/metrics"
	"github.com/prasad-ekke/buildcache/internal/protocol"
	"github.com/prasad-ekke/buildcache/internal/sockpath"
)

func main() {
	dir := flag.String("dir", defaultDir(), "cache directory for direct mode (ignored if a cacheserver is reached)")
	socket := flag.String("socket", sockpath.Default(), "cacheserver unix socket to try before falling back to direct disk mode")
	noServer := flag.Bool("no-server", false, "always run in direct disk mode; skip trying the cacheserver socket")
	gc := flag.Bool("gc", false, "run one eviction sweep against -dir and exit, instead of serving GOCACHEPROG requests -- for direct-mode users without a cacheserver to wire into cron themselves")
	maxBytes := flag.Int64("max-size", 5<<30, "used only with -gc: evict least-recently-used entries until the store is at or below this many bytes")
	stats := flag.Bool("stats", false, "print a one-line hit-rate summary to stderr when this build finishes; direct mode only, no effect when a cacheserver is reached (see cacheserver's -metrics-addr instead)")
	flag.Parse()

	if *gc {
		runGC(*dir, *maxBytes)
		return
	}

	if !*noServer {
		conn, err := net.DialTimeout("unix", *socket, 200*time.Millisecond)
		if err == nil {
			if err := runProxy(conn); err != nil {
				fail(err)
			}
			return
		}
		// No daemon listening (or a stale socket file): fall through to
		// direct mode below. Not an error -- this is the expected path
		// for anyone who hasn't started cacheserver.
	}

	store, err := disk.New(*dir)
	if err != nil {
		fail(err)
	}
	defer store.Close()

	var handler protocol.Handler = cas.NewHandler(store)
	var counters *metrics.Counters
	if *stats {
		counters = &metrics.Counters{}
		handler = metrics.WrapHandler(handler, counters)
	}

	srv := protocol.NewServer(os.Stdin, os.Stdout, handler)
	runErr := srv.Run()
	if counters != nil {
		printStats(counters.Snapshot())
	}
	if runErr != nil {
		fail(runErr)
	}
}

func printStats(s metrics.Snapshot) {
	fmt.Fprintf(os.Stderr, "cacheprog: %d gets (%d hits, %.1f%% hit rate), %d puts, %d bytes stored\n",
		s.Gets, s.Hits, s.HitRate()*100, s.Puts, s.PutBytes)
}

// runProxy relays bytes between the go command (stdin/stdout of this
// process) and an already-dialed cacheserver connection.
//
// It does not wait for the stdin->conn direction to finish before
// returning: per the GOCACHEPROG protocol, this program should exit as
// soon as it has replied to a "close" request, and conn->stdout returning
// is exactly that signal (cacheserver closes the connection right after
// replying to "close" -- see cmd/cacheserver's serveConn). Waiting on both
// directions instead risks a deadlock if the go command doesn't close its
// write end of stdin until *after* this process has already exited, which
// is the documented behavior, not an edge case. Worth a second look if a
// Windows/named-pipe transport is ever added, since pipe-closing semantics
// differ from Unix sockets.
func runProxy(conn net.Conn) error {
	defer conn.Close()
	go io.Copy(conn, os.Stdin)
	_, err := io.Copy(os.Stdout, conn)
	return err
}

func runGC(dir string, maxBytes int64) {
	store, err := disk.New(dir)
	if err != nil {
		fail(err)
	}
	defer store.Close()

	freed, err := store.Evict(maxBytes)
	if err != nil {
		fail(err)
	}
	fmt.Fprintf(os.Stderr, "cacheprog: evicted %d bytes from %s (cap %d bytes)\n", freed, dir, maxBytes)
}

func defaultDir() string {
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "buildcache")
	}
	return ".buildcache"
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "cacheprog: %v\n", err)
	os.Exit(1)
}
