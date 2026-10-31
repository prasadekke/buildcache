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
// cacheserver. A cacheserver daemon, if reachable, is layered in as a
// second tier (see internal/cas/tiered): local disk is checked first, and
// only a local miss reaches the daemon, with the result written through to
// local disk so the next Get for the same action is served purely
// locally. Talking to *both* every time gets GOCACHE-class speed for
// repeat local hits without giving up the shared cache's benefit for
// actions this machine has never built.
//
// Two ways to reach that daemon, mutually exclusive:
//   - Same machine: the default (or -socket) Unix socket, tried
//     automatically unless -no-server is set. Silent fallback to
//     local-only if nothing's listening -- normal for anyone who hasn't
//     started cacheserver. The socket must be owned by the current user
//     (see internal/sockpath). If the daemon turns out to serve the very
//     same directory as -dir (the default for both), cacheprog just uses
//     that directory directly: layering a store on top of itself would
//     write every object twice and check every miss twice for nothing.
//   - A different machine: -remote-addr, dialed over mTLS
//     (internal/tlsconfig) using -tls-cert/-tls-key/-tls-server-ca. Set
//     this and the Unix socket is never tried.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"os"
	"time"

	"github.com/prasadekke/buildcache/internal/cachedir"
	"github.com/prasadekke/buildcache/internal/cas"
	"github.com/prasadekke/buildcache/internal/cas/disk"
	"github.com/prasadekke/buildcache/internal/cas/remote"
	"github.com/prasadekke/buildcache/internal/cas/tiered"
	"github.com/prasadekke/buildcache/internal/metrics"
	"github.com/prasadekke/buildcache/internal/protocol"
	"github.com/prasadekke/buildcache/internal/sockpath"
	"github.com/prasadekke/buildcache/internal/tlsconfig"
)

func main() {
	dir := flag.String("dir", cachedir.Default(), "local cache directory, always used as at least the local tier")
	socket := flag.String("socket", sockpath.Default(), "cacheserver unix socket to layer in as a second tier; ignored if -no-server or -remote-addr is set, or if nothing is listening")
	noServer := flag.Bool("no-server", false, "never try the cacheserver socket; local disk tier only")
	remoteAddr := flag.String("remote-addr", "", "if set, connect to a remote cacheserver at this TCP address over mTLS instead of trying the local Unix socket (e.g. cache.example.com:9000); requires -tls-cert, -tls-key, -tls-server-ca")
	tlsCert := flag.String("tls-cert", "", "client TLS certificate (PEM); required if -remote-addr is set")
	tlsKey := flag.String("tls-key", "", "client TLS private key (PEM); required if -remote-addr is set")
	tlsServerCA := flag.String("tls-server-ca", "", "CA certificate (PEM) used to verify the remote cacheserver's certificate; required if -remote-addr is set")
	remoteTimeout := flag.Duration("remote-timeout", 10*time.Second, "give up on the cacheserver (and continue with local disk only) once a connection attempt, or any request in progress, makes no progress for this long")
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

	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "cacheprog: "+format+"\n", args...)
	}

	var store cas.Store = localStore
	switch {
	case *remoteAddr != "":
		// A misconfigured or unreachable remote degrades to local-only,
		// the same as a missing local daemon does -- a cache tier should
		// never be able to fail an otherwise-working build, whether
		// that's a network hiccup or a bad cert path. Loud on stderr so
		// it's visible in CI logs, but never fatal.
		tlsCfg, tlsErr := tlsconfig.Client(*tlsCert, *tlsKey, *tlsServerCA)
		if tlsErr != nil {
			logf("invalid TLS config for -remote-addr, continuing with local disk only: %v", tlsErr)
			break
		}
		if remoteStore, err := remote.DialTLS(*remoteAddr, tlsCfg, *remoteTimeout); err != nil {
			logf("connecting to remote cacheserver at %s, continuing with local disk only: %v", *remoteAddr, err)
		} else {
			store = &tiered.Store{Local: localStore, Remote: remoteStore, Logf: logf}
		}
	case !*noServer:
		if remoteStore := dialLocalDaemon(*socket, *dir, *remoteTimeout, logf); remoteStore != nil {
			store = &tiered.Store{Local: localStore, Remote: remoteStore, Logf: logf}
		}
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

// dialLocalDaemon connects to a same-machine cacheserver at socket, or
// returns nil to mean "local disk only". Every failure degrades to nil
// rather than failing the build; only the unexpected ones are logged.
func dialLocalDaemon(socket, dir string, timeout time.Duration, logf func(string, ...any)) *remote.Store {
	if err := sockpath.CheckOwner(socket); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			// Someone else's socket (or not a socket at all): never trust
			// it, since its DiskPaths would go straight into the build.
			logf("not using cacheserver socket, continuing with local disk only: %v", err)
		}
		// Otherwise no daemon: the expected path for anyone who hasn't
		// started cacheserver, so not worth a word.
		return nil
	}
	conn, err := net.DialTimeout("unix", socket, 200*time.Millisecond)
	if err != nil {
		return nil // a stale socket file with nothing listening
	}
	remoteStore, err := remote.NewConn(conn, timeout)
	if err != nil {
		// Handshake failed (e.g. something odd is listening on that path
		// that isn't a cacheserver, or one from an incompatible version):
		// degrade to local-only rather than failing the whole build over
		// it. NewConn has already closed conn.
		logf("cacheserver handshake failed, continuing with local disk only: %v", err)
		return nil
	}
	if sameDir(remoteStore.StoreDir(), dir) {
		remoteStore.Close()
		return nil
	}
	return remoteStore
}

// sameDir reports whether a and b name the same existing directory.
func sameDir(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "cacheprog: %v\n", err)
	os.Exit(1)
}
