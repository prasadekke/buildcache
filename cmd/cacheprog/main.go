// Command cacheprog is the GOCACHEPROG subprocess the go command invokes
// directly. Point the go command at it with:
//
//	GOCACHEPROG="/path/to/cacheprog" go build ./...
//
// Requires the *invoking* go command to be 1.24+ (that's what defines
// GOCACHEPROG); this binary itself builds with any reasonably modern Go
// toolchain, since it only depends on the standard library.
//
// cacheprog always opens its own local disk.Store (see internal/cas/disk)
// and serves the GOCACHEPROG protocol against it directly -- that alone is
// enough for cacheprog to work with zero setup even if nobody ever starts
// cacheserver. If a cacheserver daemon is also reachable on the default
// (or -socket) Unix socket, its store is layered in as a second tier (see
// internal/cas/tiered): local disk is checked first, and only a local miss
// reaches the daemon, with the result written through to local disk so the
// next Get for the same action is served purely locally. This replaces the
// older "either talk to the daemon, or go fully local" byte-relay proxy
// mode -- talking to *both* every time gets GOCACHE-class speed for
// repeat local hits without giving up the shared cache's benefit for
// actions this machine has never built.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/prasadekke/buildcache/internal/cas"
	"github.com/prasadekke/buildcache/internal/cas/disk"
	"github.com/prasadekke/buildcache/internal/cas/remote"
	"github.com/prasadekke/buildcache/internal/cas/tiered"
	"github.com/prasadekke/buildcache/internal/metrics"
	"github.com/prasadekke/buildcache/internal/protocol"
	"github.com/prasadekke/buildcache/internal/sockpath"
)

func main() {
	dir := flag.String("dir", defaultDir(), "local cache directory, always used as at least the local tier")
	socket := flag.String("socket", sockpath.Default(), "cacheserver unix socket to layer in as a second tier; ignored if -no-server is set or nothing is listening")
	noServer := flag.Bool("no-server", false, "never try the cacheserver socket; local disk tier only")
	gc := flag.Bool("gc", false, "run one eviction sweep against -dir and exit, instead of serving GOCACHEPROG requests -- for users without a cacheserver to wire into cron themselves")
	maxBytes := flag.Int64("max-size", 5<<30, "used only with -gc: evict least-recently-used entries until the store is at or below this many bytes")
	stats := flag.Bool("stats", false, "print a one-line hit-rate summary to stderr when this build finishes (see cacheserver's -metrics-addr for the daemon-side equivalent)")
	flag.Parse()

	if *gc {
		runGC(*dir, *maxBytes)
		return
	}

	localStore, err := disk.New(*dir)
	if err != nil {
		fail(err)
	}

	var store cas.Store = localStore
	if !*noServer {
		if conn, dialErr := net.DialTimeout("unix", *socket, 200*time.Millisecond); dialErr == nil {
			if remoteStore, err := remote.New(conn); err != nil {
				// Handshake failed (e.g. something odd is listening on that
				// path that isn't a cacheserver): degrade to local-only
				// rather than failing the whole build over it.
				conn.Close()
				fmt.Fprintf(os.Stderr, "cacheprog: cacheserver handshake failed, continuing with local disk only: %v\n", err)
			} else {
				store = &tiered.Store{
					Local:  localStore,
					Remote: remoteStore,
					Logf: func(format string, args ...any) {
						fmt.Fprintf(os.Stderr, "cacheprog: "+format+"\n", args...)
					},
				}
			}
		}
		// No daemon listening (or a stale socket file): fall through to
		// local-only below. Not an error -- this is the expected path for
		// anyone who hasn't started cacheserver.
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
