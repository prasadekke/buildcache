// Command cacheserver is the long-lived daemon behind buildcache's "server
// mode": it holds one cas.Store open and serves GOCACHEPROG requests over
// a Unix domain socket, so a warm store survives across many short-lived
// cacheprog invocations instead of being reopened by every `go build`
// subprocess.
//
// The wire protocol spoken on each connection is exactly internal/protocol
// -- the same one cacheprog speaks over stdio -- so a connection to
// cacheserver is protocol-compatible with a direct GOCACHEPROG subprocess,
// just multiplexed over a socket instead of process stdio.
//
// Known limitation: Unix domain sockets only, so this doesn't run on
// Windows yet (see internal/sockpath).
package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/prasad-ekke/buildcache/internal/cas"
	"github.com/prasad-ekke/buildcache/internal/cas/disk"
	"github.com/prasad-ekke/buildcache/internal/metrics"
	"github.com/prasad-ekke/buildcache/internal/protocol"
	"github.com/prasad-ekke/buildcache/internal/sockpath"
)

func main() {
	dir := flag.String("dir", defaultDir(), "directory to store cache entries in")
	socketPath := flag.String("socket", sockpath.Default(), "unix socket to listen on")
	maxBytes := flag.Int64("max-size", 5<<30, "evict least-recently-used entries once total cache size exceeds this many bytes (0 disables eviction)")
	gcInterval := flag.Duration("gc-interval", 10*time.Minute, "how often to run an eviction sweep")
	metricsAddr := flag.String("metrics-addr", "", "if set, serve a hit-rate dashboard (\"/\") and Prometheus-format metrics (\"/metrics\") on this address, e.g. \":9090\"")
	flag.Parse()

	store, err := disk.New(*dir)
	if err != nil {
		log.Fatalf("cacheserver: %v", err)
	}
	defer store.Close()

	if *maxBytes > 0 {
		go runGC(store, *maxBytes, *gcInterval)
	}

	// One Handler wraps the one shared Store for the whole process
	// lifetime. Handler intentionally has no Close of its own (see
	// internal/cas/handler.go) -- otherwise a single connection's "close"
	// request would tear down the store out from under every other
	// concurrent connection.
	//
	// Counters is cheap enough to run unconditionally (a few atomic adds
	// per request) so hit-rate data is always available even if
	// -metrics-addr is never set to expose it.
	counters := &metrics.Counters{}
	handler := metrics.WrapHandler(cas.NewHandler(store), counters)

	if *metricsAddr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/", counters.ServeDashboard)
		mux.HandleFunc("/metrics", counters.ServePrometheus)
		go func() {
			if err := http.ListenAndServe(*metricsAddr, mux); err != nil {
				log.Printf("cacheserver: metrics server: %v", err)
			}
		}()
		log.Printf("cacheserver: metrics dashboard at http://%s/", *metricsAddr)
	}

	if err := os.Remove(*socketPath); err != nil && !os.IsNotExist(err) {
		log.Fatalf("cacheserver: removing stale socket %s: %v", *socketPath, err)
	}
	ln, err := net.Listen("unix", *socketPath)
	if err != nil {
		log.Fatalf("cacheserver: listening on %s: %v", *socketPath, err)
	}
	defer ln.Close()
	log.Printf("cacheserver: listening on %s (cache dir %s)", *socketPath, *dir)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Printf("cacheserver: shutting down")
		ln.Close()
		os.Remove(*socketPath)
		os.Exit(0)
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("cacheserver: accept: %v", err)
			return
		}
		go serveConn(conn, handler)
	}
}

// serveConn runs one GOCACHEPROG protocol session over conn. Every
// connection shares the same underlying handler/store; concurrency safety
// for that sharing lives in the Store implementation (see disk.Store's
// atomic-write comment), not here.
func serveConn(conn net.Conn, h protocol.Handler) {
	defer conn.Close()
	srv := protocol.NewServer(conn, conn, h)
	if err := srv.Run(); err != nil {
		log.Printf("cacheserver: connection error: %v", err)
	}
}

// runGC runs an eviction sweep immediately (useful after the daemon was
// offline for a while and the cache grew unchecked in direct-mode
// fallback), then on the given interval for as long as the process runs.
func runGC(store *disk.Store, maxBytes int64, interval time.Duration) {
	sweep := func() {
		freed, err := store.Evict(maxBytes)
		if err != nil {
			log.Printf("cacheserver: eviction sweep failed: %v", err)
			return
		}
		if freed > 0 {
			log.Printf("cacheserver: eviction freed %d bytes", freed)
		}
	}

	sweep()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		sweep()
	}
}

func defaultDir() string {
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "buildcache")
	}
	return ".buildcache"
}
