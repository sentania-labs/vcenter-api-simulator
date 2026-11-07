package serve

import (
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// rotate writes a fresh self-signed pair into dir with a modification time
// after what is there, the way a replaced Secret mount or a renewal lands.
func rotate(t *testing.T, dir string, when time.Time) *big.Int {
	t.Helper()
	certPEM, keyPEM, err := selfSigned([]string{"localhost", "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"cert.pem": certPEM, "key.pem": keyPEM} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
	}
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.SerialNumber
}

// served dials the listener and returns the serial of the certificate it
// presents.
func served(t *testing.T, addr string) *big.Int {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // identity check, not trust
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0].SerialNumber
}

// A replaced pair is served to the next handshake without a restart; a
// half-replaced or broken pair keeps the current one until the replacement
// is complete.
func TestCertificateRotation(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-time.Hour)
	first := rotate(t, dir, base)

	cert, err := LoadOrCreateCertificate(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	cert.Interval = 0 // check the files on every handshake
	if got := cert.Current().Leaf.SerialNumber; got.Cmp(first) != 0 {
		t.Fatalf("loaded serial %s, want %s", got, first)
	}

	ls, err := Listen([]string{"127.0.0.1:0"}, nil, cert)
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Close()
	addr := ls.Addrs[0]
	if got := served(t, addr); got.Cmp(first) != 0 {
		t.Fatalf("served serial %s before rotation, want %s", got, first)
	}

	second := rotate(t, dir, base.Add(time.Minute))
	if got := served(t, addr); got.Cmp(second) != 0 {
		t.Fatalf("served serial %s after rotation, want %s", got, second)
	}

	// Half a rotation: a new certificate whose key has not landed yet.
	certPEM, _, err := selfSigned([]string{"localhost"})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "cert.pem")
	if err := os.WriteFile(p, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, base.Add(2*time.Minute), base.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := served(t, addr); got.Cmp(second) != 0 {
		t.Fatalf("served serial %s during a half rotation, want the previous %s", got, second)
	}

	third := rotate(t, dir, base.Add(3*time.Minute))
	if got := served(t, addr); got.Cmp(third) != 0 {
		t.Fatalf("served serial %s after the completed rotation, want %s", got, third)
	}
}

// The first start creates a pair and a second start loads the same one.
func TestLoadOrCreateCertificatePersists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	a, err := LoadOrCreateCertificate(dir, []string{"vcsim.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateCertificate(dir, []string{"other.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Current().Leaf.SerialNumber.Cmp(b.Current().Leaf.SerialNumber) != 0 {
		t.Fatal("a second start issued a new certificate")
	}
	if names := a.Current().Leaf.DNSNames; len(names) != 1 || names[0] != "vcsim.example.test" {
		t.Fatalf("DNS names %v", names)
	}
	if err := os.Remove(filepath.Join(dir, "key.pem")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateCertificate(dir, nil); err == nil {
		t.Fatal("half a pair was accepted")
	}
}
