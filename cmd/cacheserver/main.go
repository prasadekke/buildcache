// Command cacheserver is the long-lived daemon behind buildcache's "server
// mode": it holds one cas.Store open and serves GOCACHEPROG requests over
// a Unix domain socket, so a warm store survives across many short-lived
// cacheprog invocations instead of being reopened by every `go build`
// subprocess. If -tcp-addr is also set, it listens there too, over mTLS
// (internal/tlsconfig) -- that's the transport for cacheprog instances on
// other machines, since the Unix socket is inherently same-machine-only.
//
// The wire protocol spoken on each connection is exactly internal/protocol
// -- the same one cacheprog speaks over stdio -- so a connection to
// cacheserver is protocol-compatible with a direct GOCACHEPROG subprocess,
// just multiplexed over a socket instead of process stdio. TCP connections
// additionally get their Get responses wrapped with cas.InlineGetBodies:
// a "get" hit's DiskPath is meaningless to a caller that doesn't share
// cacheserver's filesystem, so those connections get the object's bytes
// inlined instead (see internal/cas/remote.Store, which reads them back
// out on the other end). The Unix socket skips that wrapping -- DiskPath
// is already directly valid there, and inlining would just be wasted I/O.
//
// Every "put", on either transport, is checked against its OutputID before
// it's stored (cas.VerifyPuts). TCP connections are additionally bounded:
// an inactivity timeout, a cap on concurrent connections and on object
// size, and optionally read-only access for clients whose certificate was
// issued by -tls-readonly-client-ca.
//
// Known limitation: the Unix socket won't run on Windows (see
// internal/sockpath); the TCP+mTLS listener isn't platform-limited.
package main

import (
	"crypto/tls"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/prasadekke/buildcache/internal/cachedir"
	"github.com/prasadekke/buildcache/internal/cas"
	"github.com/prasadekke/buildcache/internal/cas/disk"
	"github.com/prasadekke/buildcache/internal/idleconn"
	"github.com/prasadekke/buildcache/internal/metrics"
	"github.com/prasadekke/buildcache/internal/protocol"
	"github.com/prasadekke/buildcache/internal/sockpath"
	"github.com/prasadekke/buildcache/internal/tlsconfig"
)

// maxHandshake bounds the TLS handshake on a new -tcp-addr connection, so
// a client that connects and then says nothing can't hold a connection
// slot until the (much longer) idle timeout.
const maxHandshake = 10 * time.Second

func main() {
	dir := flag.String("dir", cachedir.Default(), "directory to store cache entries in")
	socketPath := flag.String("socket", sockpath.Default(), "unix socket to listen on")
	maxBytes := flag.Int64("max-size", 5<<30, "evict least-recently-used entries once total cache size exceeds this many bytes (0 disables eviction)")
	gcInterval := flag.Duration("gc-interval", 10*time.Minute, "how often to run an eviction sweep")
	metricsAddr := flag.String("metrics-addr", "", "if set, serve a hit-rate dashboard (\"/\") and Prometheus-format metrics (\"/metrics\") on this address, e.g. \":9090\"")
	tcpAddr := flag.String("tcp-addr", "", "if set, also listen for GOCACHEPROG connections here over mTLS (e.g. \":9000\") -- for cacheprog instances on other machines; requires -tls-cert, -tls-key, -tls-client-ca")
	tlsCert := flag.String("tls-cert", "", "server TLS certificate (PEM); required if -tcp-addr is set")
	tlsKey := flag.String("tls-key", "", "server TLS private key (PEM); required if -tcp-addr is set")
	tlsClientCA := flag.String("tls-client-ca", "", "CA certificate (PEM) used to verify client certificates; required if -tcp-addr is set")
	tlsReadOnlyCA := flag.String("tls-readonly-client-ca", "", "optional CA certificate (PEM) whose client certificates get read-only access over -tcp-addr; -tls-client-ca clients keep read-write access")
	idleTimeout := flag.Duration("idle-timeout", 10*time.Minute, "close a -tcp-addr connection once no data has moved on it for this long (0 disables)")
	maxConns := flag.Int("max-conns", 256, "maximum concurrent -tcp-addr connections; further clients wait to be accepted")
	maxObjectSize := flag.Int64("max-object-size", 1<<30, "largest object a -tcp-addr client may put, in bytes (0 = unlimited)")
	shutdownTimeout := flag.Duration("shutdown-timeout", 10*time.Second, "on SIGINT/SIGTERM, how long to let requests already in progress finish before closing their connections")
	flag.Parse()

	absDir, err := filepath.Abs(*dir)
	if err != nil {
		log.Fatalf("cacheserver: %v", err)
	}
	store, err := disk.New(absDir)
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
	// -metrics-addr is never set to expose it. Both listeners share it,
	// for one aggregate view of hit rate regardless of transport.
	baseHandler := cas.VerifyPuts(cas.NewHandler(store))
	counters := &metrics.Counters{}
	unixHandler := metrics.WrapHandler(baseHandler, counters)

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

	conns := newConnTracker()

	if *socketPath == sockpath.Default() {
		// The default path lives under a shared temp dir; make sure its
		// per-user directory is ours and private before listening in it.
		// See internal/sockpath for why that matters.
		if err := sockpath.PrepareDir(*socketPath); err != nil {
			log.Fatalf("cacheserver: %v", err)
		}
	}
	if c, err := net.DialTimeout("unix", *socketPath, 200*time.Millisecond); err == nil {
		// Not stale: removing it would silently take over from a running
		// daemon and strand every client connected to it.
		c.Close()
		log.Fatalf("cacheserver: another process is already listening on %s", *socketPath)
	}
	if err := sockpath.RemoveStale(*socketPath); err != nil {
		log.Fatalf("cacheserver: removing stale socket: %v", err)
	}
	unixLn, err := net.Listen("unix", *socketPath)
	if err != nil {
		log.Fatalf("cacheserver: listening on %s: %v", *socketPath, err)
	}
	if err := os.Chmod(*socketPath, 0o600); err != nil {
		log.Fatalf("cacheserver: restricting %s to its owner: %v", *socketPath, err)
	}
	log.Printf("cacheserver: listening on %s (cache dir %s)", *socketPath, absDir)
	go acceptLoop(unixLn, nil, conns, "unix:"+*socketPath, func(conn net.Conn) {
		// Same machine, same user (see the socket's permissions): DiskPath
		// is directly readable, and advertising the store directory lets a
		// cacheprog using this very directory skip layering it on itself.
		srv := protocol.NewServer(conn, conn, unixHandler)
		srv.StoreDir = absDir
		runSession(srv, conns)
	})

	var tcpLn net.Listener
	if *tcpAddr != "" {
		var (
			tlsCfg     *tls.Config
			isReadOnly = func(tls.ConnectionState) bool { return false }
			err        error
		)
		if *tlsReadOnlyCA != "" {
			tlsCfg, isReadOnly, err = tlsconfig.ServerWithReadOnly(*tlsCert, *tlsKey, *tlsClientCA, *tlsReadOnlyCA)
		} else {
			tlsCfg, err = tlsconfig.Server(*tlsCert, *tlsKey, *tlsClientCA)
		}
		if err != nil {
			log.Fatalf("cacheserver: -tcp-addr set but TLS config is invalid: %v", err)
		}
		ln, err := tls.Listen("tcp", *tcpAddr, tlsCfg)
		if err != nil {
			log.Fatalf("cacheserver: listening on %s: %v", *tcpAddr, err)
		}
		tcpLn = ln
		tcpHandler := metrics.WrapHandler(cas.InlineGetBodies(baseHandler), counters)
		log.Printf("cacheserver: listening on %s (mTLS)", *tcpAddr)

		slots := make(chan struct{}, max(*maxConns, 1))
		go acceptLoop(tcpLn, slots, conns, "tcp:"+*tcpAddr, func(conn net.Conn) {
			tlsConn := conn.(*tls.Conn)
			if err := handshake(tlsConn, *idleTimeout); err != nil {
				log.Printf("cacheserver: %s: TLS handshake: %v", conn.RemoteAddr(), err)
				return
			}
			wrapped := idleconn.Wrap(tlsConn, *idleTimeout)
			srv := protocol.NewServer(wrapped, wrapped, tcpHandler)
			srv.ReadOnly = isReadOnly(tlsConn.ConnectionState())
			srv.MaxBodySize = *maxObjectSize
			runSession(srv, conns)
		})
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	log.Printf("cacheserver: shutting down")
	conns.beginShutdown()
	unixLn.Close()
	os.Remove(*socketPath)
	if tcpLn != nil {
		tcpLn.Close()
	}
	conns.drain(*shutdownTimeout)
}

// handshake completes conn's TLS handshake within a bounded time, so the
// client's certificate (and with it, its role) is known before serving it.
func handshake(conn *tls.Conn, idleTimeout time.Duration) error {
	timeout := maxHandshake
	if idleTimeout > 0 && idleTimeout < timeout {
		timeout = idleTimeout
	}
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	if err := conn.Handshake(); err != nil {
		return err
	}
	return conn.SetDeadline(time.Time{})
}

// acceptLoop accepts connections on ln until it errors (typically because
// it was closed for shutdown) and serves each one in its own goroutine. If
// slots is non-nil, it caps how many connections are served at once:
// acceptLoop waits for a free slot before accepting the next connection,
// leaving further clients queued in the kernel's backlog.
func acceptLoop(ln net.Listener, slots chan struct{}, conns *connTracker, label string, serve func(net.Conn)) {
	for {
		if slots != nil {
			slots <- struct{}{}
		}
		conn, err := ln.Accept()
		if err != nil {
			if !conns.shuttingDown() {
				log.Printf("cacheserver: %s: accept: %v", label, err)
			}
			return
		}
		if !conns.add(conn) {
			conn.Close()
			return
		}
		go func() {
			defer func() {
				conn.Close()
				conns.remove(conn)
				if slots != nil {
					<-slots
				}
			}()
			serve(conn)
		}()
	}
}

// runSession runs one GOCACHEPROG protocol session. Every connection
// shares the same underlying handler/store; concurrency safety for that
// sharing lives in the Store implementation (see disk.Store's
// atomic-write comment), not here.
func runSession(srv *protocol.Server, conns *connTracker) {
	if err := srv.Run(); err != nil && !conns.shuttingDown() {
		log.Printf("cacheserver: connection error: %v", err)
	}
}

// connTracker keeps track of open connections, so shutdown can let
// requests already in progress finish instead of cutting them off.
type connTracker struct {
	mu      sync.Mutex
	conns   map[net.Conn]struct{}
	closing bool
	wg      sync.WaitGroup
}

func newConnTracker() *connTracker {
	return &connTracker{conns: make(map[net.Conn]struct{})}
}

// add registers conn, or reports false if shutdown has already begun.
func (t *connTracker) add(conn net.Conn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closing {
		return false
	}
	t.conns[conn] = struct{}{}
	t.wg.Add(1)
	return true
}

func (t *connTracker) remove(conn net.Conn) {
	t.mu.Lock()
	delete(t.conns, conn)
	t.mu.Unlock()
	t.wg.Done()
}

func (t *connTracker) shuttingDown() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closing
}

// beginShutdown stops new connections from being registered and makes
// every open one stop reading: a connection idle between requests ends
// right away, while one partway through answering a request still gets to
// write its response, then ends on its next read.
func (t *connTracker) beginShutdown() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closing = true
	now := time.Now()
	for c := range t.conns {
		c.SetReadDeadline(now)
	}
}

// drain waits up to timeout for open connections to finish after
// beginShutdown, then closes whatever is left.
func (t *connTracker) drain(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return
	case <-time.After(timeout):
	}
	t.mu.Lock()
	log.Printf("cacheserver: closing %d connection(s) still open after %v", len(t.conns), timeout)
	for c := range t.conns {
		c.Close()
	}
	t.mu.Unlock()
	<-done
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
