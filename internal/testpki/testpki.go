// Package testpki generates throwaway certificate material for tests that
// exercise buildcache's mTLS transport (internal/tlsconfig), so those
// tests don't depend on any real PKI or checked-in certificate fixtures.
// Not for production use.
package testpki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// CA is a throwaway certificate authority for issuing test leaf
// certificates that all verify against the same root.
type CA struct {
	cert   *x509.Certificate
	der    []byte
	key    *ecdsa.PrivateKey
	dir    string
	nextSN int64
}

// NewCA generates a fresh CA, valid for the duration of the test.
func NewCA(t *testing.T) *CA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating CA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "buildcache-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating CA certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing CA certificate: %v", err)
	}
	return &CA{cert: cert, der: der, key: key, dir: t.TempDir(), nextSN: 2}
}

// File writes the CA's own certificate to a PEM file and returns its path
// -- this is what a peer verifies leaf certificates against.
func (ca *CA) File(t *testing.T) string {
	t.Helper()
	path := filepath.Join(ca.dir, "ca.pem")
	writePEMFile(t, path, "CERTIFICATE", ca.der)
	return path
}

// Issue mints a new leaf certificate signed by ca, valid for localhost and
// 127.0.0.1/::1 (fine for either server or client use in a test), and
// returns the paths to its PEM cert and key files.
func (ca *CA) Issue(t *testing.T, cn string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key for %s: %v", cn, err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(ca.nextSN),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	ca.nextSN++
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("creating certificate for %s: %v", cn, err)
	}

	certFile = filepath.Join(ca.dir, cn+"-cert.pem")
	keyFile = filepath.Join(ca.dir, cn+"-key.pem")
	writePEMFile(t, certFile, "CERTIFICATE", der)

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshaling key for %s: %v", cn, err)
	}
	writePEMFile(t, keyFile, "EC PRIVATE KEY", keyDER)
	return certFile, keyFile
}

func writePEMFile(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating %s: %v", path, err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: blockType, Bytes: der}); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
