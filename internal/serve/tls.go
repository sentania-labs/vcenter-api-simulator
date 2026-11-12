// Package serve runs the simulator's public HTTPS listeners with a
// certificate that persists across restarts. The 2.0.0 management pack pins
// the SHA-256 of the certificate an operator accepted, so a certificate that
// changed on every start would break collection until re-accepted (recon
// risk 4).
package serve

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Certificate is the key pair served from a directory. It notices when the
// files are replaced (a rotated Kubernetes Secret, a cert-manager renewal)
// and serves the new pair to the next handshake, without a restart. A pair
// that does not load, or loads half-replaced, keeps the current one.
type Certificate struct {
	certFile, keyFile string
	// Interval is the least time between checks of the files; zero checks on
	// every handshake.
	Interval time.Duration

	mu         sync.Mutex
	current    *tls.Certificate
	certMod    time.Time
	keyMod     time.Time
	checked    time.Time
	managed    bool
	consoleDir string
}

// LoadOrCreateCertificate returns the key pair in dir, creating a
// self-signed one for the given names on first use.
func LoadOrCreateCertificate(dir string, names []string) (*Certificate, error) {
	c := &Certificate{
		certFile: filepath.Join(dir, "cert.pem"),
		keyFile:  filepath.Join(dir, "key.pem"),
		Interval: time.Second,
	}
	err := c.load()
	if err == nil {
		return c, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("tls: load %s: %w", dir, err)
	}
	// Never replace half a pair: the pinned certificate would change silently.
	_, certErr := os.Stat(c.certFile)
	_, keyErr := os.Stat(c.keyFile)
	if certErr == nil || keyErr == nil {
		return nil, fmt.Errorf("tls: %s has only one of cert.pem and key.pem; restore the other or remove both", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	certPEM, keyPEM, err := selfSigned(names)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(c.keyFile, keyPEM, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(c.certFile, certPEM, 0o644); err != nil {
		return nil, err
	}
	if err := c.load(); err != nil {
		return nil, fmt.Errorf("tls: load %s: %w", dir, err)
	}
	return c, nil
}

// LoadCertificateFiles serves a provided key pair (an operator's or a
// mounted Secret's certFile and keyFile). Like LoadOrCreateCertificate's
// pair it notices when the files are replaced and serves the new pair to
// the next handshake. It never creates or writes anything.
func LoadCertificateFiles(certFile, keyFile string) (*Certificate, error) {
	c := &Certificate{certFile: certFile, keyFile: keyFile, Interval: time.Second}
	if err := c.load(); err != nil {
		return nil, fmt.Errorf("tls: load %s and %s: %w", certFile, keyFile, err)
	}
	return c, nil
}

// GetCertificate is the tls.Config hook; it returns the current pair after
// checking the files for a replacement.
func (c *Certificate) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return c.Current(), nil
}

// Current returns the pair in use, reloading it first when both files have
// changed since the last load.
func (c *Certificate) Current() *tls.Certificate {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.Interval > 0 && now.Sub(c.checked) < c.Interval {
		return c.current
	}
	c.checked = now
	certMod, keyMod, err := c.modTimes()
	if err != nil || (certMod.Equal(c.certMod) && keyMod.Equal(c.keyMod)) {
		return c.current
	}
	if err := c.load(); err != nil {
		// Half-rotated or broken: keep serving what worked and try again
		// when the files change next (the other half of the pair landing).
		log.Printf("tls: %s not reloaded: %v", filepath.Dir(c.certFile), err)
		c.certMod, c.keyMod = certMod, keyMod
		return c.current
	}
	log.Printf("tls: reloaded %s (serial %s)", filepath.Dir(c.certFile), c.current.Leaf.SerialNumber)
	return c.current
}

// load reads the pair and records the modification times it was read at.
// The caller holds mu, or owns c alone.
func (c *Certificate) load() error {
	certMod, keyMod, err := c.modTimes()
	if err != nil {
		return err
	}
	cert, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		return err
	}
	if cert.Leaf == nil {
		if cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return err
		}
	}
	c.current, c.certMod, c.keyMod = &cert, certMod, keyMod
	return nil
}

func (c *Certificate) modTimes() (certMod, keyMod time.Time, err error) {
	ci, err := os.Stat(c.certFile)
	if err != nil {
		return certMod, keyMod, err
	}
	ki, err := os.Stat(c.keyFile)
	if err != nil {
		return certMod, keyMod, err
	}
	return ci.ModTime(), ki.ModTime(), nil
}

// selfSigned makes a ten-year self-signed server certificate for names.
func selfSigned(names []string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "vcenter-api-simulator", Organization: []string{"vcenter-api-simulator"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if n != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, n)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}
