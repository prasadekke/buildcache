// Package tlsconfig builds the *tls.Config for buildcache's one
// network-facing transport: mTLS between cacheprog and cacheserver over
// TCP. The GOCACHEPROG wire protocol (internal/protocol) has no
// authentication of its own -- it was never meant to leave a single Unix
// socket, where filesystem permissions were the access control. Once
// cacheserver listens on a real network, mTLS is what stands between it
// and anyone on that network being able to read the cache or poison it
// with malicious build outputs, so both sides require and verify a
// certificate rather than just encrypting the connection.
package tlsconfig

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
)

// Server returns a *tls.Config for cacheserver's mTLS listener: it
// presents certFile/keyFile as its own identity, and requires every
// connecting client to present a certificate that verifies against
// clientCAFile.
func Server(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("loading server certificate: %w", err)
	}
	clientCAs, err := loadCertPool(clientCAFile)
	if err != nil {
		return nil, fmt.Errorf("loading client CA: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// ServerWithReadOnly is Server, plus a second CA whose clients may only
// read from the cache. It returns the *tls.Config, which accepts clients
// verified by either CA, and isReadOnly, which reports whether an
// established connection's client was verified only through
// readOnlyClientCAFile -- cacheserver serves such connections with "put"
// disabled (see protocol.Server.ReadOnly).
//
// Splitting roles by issuing CA rather than by some attribute of the
// client certificate means a read-only client can't promote itself: the
// only way to get a read-write certificate is to have the read-write CA
// sign one. The typical split is CI runners (which build trusted code) on
// the read-write CA and developer laptops on the read-only one, so a
// compromised laptop can't poison what CI and every other machine read.
func ServerWithReadOnly(certFile, keyFile, clientCAFile, readOnlyClientCAFile string) (cfg *tls.Config, isReadOnly func(tls.ConnectionState) bool, err error) {
	cfg, err = Server(certFile, keyFile, clientCAFile)
	if err != nil {
		return nil, nil, err
	}
	rwCerts, err := loadCerts(clientCAFile)
	if err != nil {
		return nil, nil, fmt.Errorf("loading client CA: %w", err)
	}
	roCerts, err := loadCerts(readOnlyClientCAFile)
	if err != nil {
		return nil, nil, fmt.Errorf("loading read-only client CA: %w", err)
	}

	rw := make(map[string]bool, len(rwCerts))
	for _, c := range rwCerts {
		rw[string(c.Raw)] = true
	}
	for _, c := range roCerts {
		if rw[string(c.Raw)] {
			return nil, nil, fmt.Errorf("certificate %q is in both the read-write and read-only client CA files", c.Subject)
		}
		cfg.ClientCAs.AddCert(c)
	}

	isReadOnly = func(state tls.ConnectionState) bool {
		if len(state.VerifiedChains) == 0 {
			return true // never reached under RequireAndVerifyClientCert; fail closed anyway
		}
		for _, chain := range state.VerifiedChains {
			if rw[string(chain[len(chain)-1].Raw)] {
				return false
			}
		}
		return true
	}
	return cfg, isReadOnly, nil
}

// Client returns a *tls.Config for cacheprog connecting to a remote
// cacheserver over mTLS: it presents certFile/keyFile as its own identity
// and verifies the server's certificate against serverCAFile, so cacheprog
// never sends build outputs to, or trusts cache hits from, anything but
// the specific server it's been configured to talk to.
func Client(certFile, keyFile, serverCAFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("loading client certificate: %w", err)
	}
	serverCAs, err := loadCertPool(serverCAFile)
	if err != nil {
		return nil, fmt.Errorf("loading server CA: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      serverCAs,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

func loadCerts(caFile string) ([]*x509.Certificate, error) {
	rest, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading %q: %w", caFile, err)
	}
	var certs []*x509.Certificate
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parsing certificate in %q: %w", caFile, err)
		}
		certs = append(certs, c)
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("no valid certificates found in %q", caFile)
	}
	return certs, nil
}

func loadCertPool(caFile string) (*x509.CertPool, error) {
	pemBytes, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading %q: %w", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("no valid certificates found in %q", caFile)
	}
	return pool, nil
}
