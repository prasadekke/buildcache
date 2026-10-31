package tlsconfig_test

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"os"
	"testing"

	"github.com/prasadekke/buildcache/internal/testpki"
	"github.com/prasadekke/buildcache/internal/tlsconfig"
)

// clientConfigTrusting returns a bare *tls.Config that trusts caFile as
// its root but presents no client certificate of its own -- for testing
// that the server actually enforces RequireAndVerifyClientCert.
func clientConfigTrusting(t *testing.T, caFile string) *tls.Config {
	t.Helper()
	pemBytes, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatalf("reading %s: %v", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		t.Fatalf("no certificates found in %s", caFile)
	}
	return &tls.Config{RootCAs: pool}
}

func TestServerClient_HandshakeAndMutualAuth(t *testing.T) {
	ca := testpki.NewCA(t)
	caFile := ca.File(t)
	serverCert, serverKey := ca.Issue(t, "server")
	clientCert, clientKey := ca.Issue(t, "client")

	serverTLS, err := tlsconfig.Server(serverCert, serverKey, caFile)
	if err != nil {
		t.Fatalf("tlsconfig.Server: %v", err)
	}
	clientTLS, err := tlsconfig.Client(clientCert, clientKey, caFile)
	if err != nil {
		t.Fatalf("tlsconfig.Client: %v", err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	defer ln.Close()

	serverDone := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		buf := make([]byte, 5)
		if _, err := io.ReadFull(conn, buf); err != nil {
			serverDone <- err
			return
		}
		if string(buf) != "hello" {
			serverDone <- err
			return
		}
		_, err = conn.Write([]byte("world"))
		serverDone <- err
	}()

	conn, err := tls.Dial("tcp", ln.Addr().String(), clientTLS)
	if err != nil {
		t.Fatalf("tls.Dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf) != "world" {
		t.Errorf("got %q, want %q", buf, "world")
	}
	if err := <-serverDone; err != nil {
		t.Errorf("server side: %v", err)
	}
}

func TestServer_RejectsClientWithoutCert(t *testing.T) {
	ca := testpki.NewCA(t)
	caFile := ca.File(t)
	serverCert, serverKey := ca.Issue(t, "server")

	serverTLS, err := tlsconfig.Server(serverCert, serverKey, caFile)
	if err != nil {
		t.Fatalf("tlsconfig.Server: %v", err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 1)
		conn.Read(buf) // triggers the handshake server-side; expected to fail
	}()

	// A client that trusts the real server CA but presents no client
	// certificate of its own should still be rejected, because the server
	// requires one (RequireAndVerifyClientCert).
	noClientCertTLS := clientConfigTrusting(t, caFile)
	conn, dialErr := tls.Dial("tcp", ln.Addr().String(), noClientCertTLS)
	if dialErr == nil {
		defer conn.Close()
		// The TCP-level connect can succeed before the handshake completes;
		// the first Read is what surfaces the handshake failure.
		buf := make([]byte, 1)
		if _, readErr := conn.Read(buf); readErr == nil {
			t.Fatal("expected the handshake to fail for a client with no certificate")
		}
	}
}

func TestClient_RejectsServerWithWrongCA(t *testing.T) {
	// mainCA issues both the server's cert and the client's cert, so the
	// server-side "does this client's cert check out" step would pass --
	// isolating the one thing this test actually wants to fail: the
	// client's own verification of the server's certificate, because the
	// client is (deliberately) configured to trust a different, unrelated
	// CA instead of mainCA.
	mainCA := testpki.NewCA(t)
	mainCAFile := mainCA.File(t)
	serverCert, serverKey := mainCA.Issue(t, "server")
	clientCert, clientKey := mainCA.Issue(t, "client")

	unrelatedCA := testpki.NewCA(t)
	unrelatedCAFile := unrelatedCA.File(t)

	serverTLS, err := tlsconfig.Server(serverCert, serverKey, mainCAFile)
	if err != nil {
		t.Fatalf("tlsconfig.Server: %v", err)
	}
	clientTLS, err := tlsconfig.Client(clientCert, clientKey, unrelatedCAFile)
	if err != nil {
		t.Fatalf("tlsconfig.Client: %v", err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 1)
		conn.Read(buf)
	}()

	conn, err := tls.Dial("tcp", ln.Addr().String(), clientTLS)
	if err == nil {
		conn.Close()
		t.Fatal("Dial err = nil, want a verification failure: the client trusts a different CA than issued the server's certificate")
	}
}

func TestServerWithReadOnly_ClassifiesByIssuingCA(t *testing.T) {
	rwCA := testpki.NewCA(t)
	roCA := testpki.NewCA(t)
	serverCert, serverKey := rwCA.Issue(t, "server")

	serverTLS, isReadOnly, err := tlsconfig.ServerWithReadOnly(serverCert, serverKey, rwCA.File(t), roCA.File(t))
	if err != nil {
		t.Fatalf("ServerWithReadOnly: %v", err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	defer ln.Close()

	for _, tc := range []struct {
		name     string
		ca       *testpki.CA
		readOnly bool
	}{
		{"read-write client", rwCA, false},
		{"read-only client", roCA, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cert, key := tc.ca.Issue(t, "client")
			clientTLS, err := tlsconfig.Client(cert, key, rwCA.File(t))
			if err != nil {
				t.Fatalf("tlsconfig.Client: %v", err)
			}

			got := make(chan bool, 1)
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					got <- false
					return
				}
				defer conn.Close()
				tlsConn := conn.(*tls.Conn)
				if err := tlsConn.Handshake(); err != nil {
					t.Errorf("server handshake: %v", err)
					got <- false
					return
				}
				got <- isReadOnly(tlsConn.ConnectionState())
			}()

			conn, err := tls.Dial("tcp", ln.Addr().String(), clientTLS)
			if err != nil {
				t.Fatalf("tls.Dial: %v", err)
			}
			defer conn.Close()
			if err := conn.Handshake(); err != nil {
				t.Fatalf("client handshake: %v", err)
			}
			if ro := <-got; ro != tc.readOnly {
				t.Errorf("isReadOnly = %v, want %v", ro, tc.readOnly)
			}
		})
	}
}

func TestServerWithReadOnly_RejectsCAInBothRoles(t *testing.T) {
	ca := testpki.NewCA(t)
	cert, key := ca.Issue(t, "server")
	if _, _, err := tlsconfig.ServerWithReadOnly(cert, key, ca.File(t), ca.File(t)); err == nil {
		t.Fatal("ServerWithReadOnly err = nil, want a refusal for one CA in both roles")
	}
}
