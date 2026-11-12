package serve

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var ErrCertificateManaged = errors.New("certificate is managed by the deployment")

type CertificateInfo struct {
	Names       []string  `json:"names"`
	Fingerprint string    `json:"fingerprint"`
	Expires     time.Time `json:"expires"`
	Managed     bool      `json:"managed"`
	// Issuer is who signed the certificate; SelfSigned is true when it
	// signed itself, as the certificates the simulator makes do.
	Issuer     string `json:"issuer"`
	SelfSigned bool   `json:"selfSigned"`
}

func describe(leaf *x509.Certificate) CertificateInfo {
	names := append([]string{}, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		names = append(names, ip.String())
	}
	if len(names) == 0 && leaf.Subject.CommonName != "" {
		names = append(names, leaf.Subject.CommonName)
	}
	hash := sha256.Sum256(leaf.Raw)
	issuer := leaf.Issuer.CommonName
	if issuer == "" && len(leaf.Issuer.Organization) > 0 {
		issuer = leaf.Issuer.Organization[0]
	}
	return CertificateInfo{Names: names, Fingerprint: hex.EncodeToString(hash[:]), Expires: leaf.NotAfter.UTC(),
		Issuer: issuer, SelfSigned: string(leaf.RawIssuer) == string(leaf.RawSubject)}
}

// Inspect reads a certificate, and its key when one is given, without
// using either: what the certificate is valid for, who issued it, when it
// runs out, and whether the key belongs to it. keyGiven false leaves
// keyMatches false.
func Inspect(certPEM, keyPEM []byte) (info CertificateInfo, keyMatches bool, err error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return CertificateInfo{}, false, errors.New("not a certificate")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return CertificateInfo{}, false, errors.New("not a certificate")
	}
	info = describe(leaf)
	if len(keyPEM) > 0 {
		_, pairErr := tls.X509KeyPair(certPEM, keyPEM)
		keyMatches = pairErr == nil
	}
	return info, keyMatches, nil
}

func (c *Certificate) MarkManaged() { c.mu.Lock(); defer c.mu.Unlock(); c.managed = true }
func (c *Certificate) Info() CertificateInfo {
	cert := c.Current()
	c.mu.Lock()
	managed := c.managed
	c.mu.Unlock()
	info := describe(cert.Leaf)
	info.Managed = managed
	return info
}

type certificateState struct {
	Generation string `json:"generation"`
}

// EnableConsole selects a previously uploaded or generated pair, if present.
// A deployment-managed certificate never calls this and always takes priority.
func (c *Certificate) EnableConsole(dir string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.consoleDir = filepath.Join(dir, "console-certificate")
	b, err := os.ReadFile(filepath.Join(c.consoleDir, "active.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var state certificateState
	if err = json.Unmarshal(b, &state); err != nil {
		return err
	}
	if state.Generation == "" || filepath.Base(state.Generation) != state.Generation || state.Generation == "." || state.Generation == ".." {
		return errors.New("invalid saved certificate generation")
	}
	oldCert, oldKey := c.certFile, c.keyFile
	c.certFile = filepath.Join(c.consoleDir, state.Generation, "cert.pem")
	c.keyFile = filepath.Join(c.consoleDir, state.Generation, "key.pem")
	if err = c.load(); err != nil {
		c.certFile, c.keyFile = oldCert, oldKey
		return fmt.Errorf("load console certificate: %w", err)
	}
	return nil
}

// Replace validates and persists an entire key pair before switching the active
// generation. No private-key material is returned to the caller.
func (c *Certificate) Replace(certPEM, keyPEM []byte) error {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return errors.New("certificate and private key do not form a valid pair")
	}
	if len(cert.Certificate) == 0 {
		return errors.New("certificate is empty")
	}
	cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return errors.New("certificate is invalid")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.managed {
		return ErrCertificateManaged
	}
	if c.consoleDir == "" {
		return errors.New("console certificate changes are unavailable")
	}
	if err = os.MkdirAll(c.consoleDir, 0700); err != nil {
		return err
	}
	gen, err := os.MkdirTemp(c.consoleDir, "generation-")
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(gen)
		}
	}()
	if err = os.WriteFile(filepath.Join(gen, "key.pem"), keyPEM, 0600); err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(gen, "cert.pem"), certPEM, 0600); err != nil {
		return err
	}
	certInfo, err := os.Stat(filepath.Join(gen, "cert.pem"))
	if err != nil {
		return err
	}
	keyInfo, err := os.Stat(filepath.Join(gen, "key.pem"))
	if err != nil {
		return err
	}
	state, _ := json.Marshal(certificateState{Generation: filepath.Base(gen)})
	f, err := os.CreateTemp(c.consoleDir, ".active-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(state); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(c.consoleDir, "active.json")); err != nil {
		return err
	}
	committed = true
	c.certFile, c.keyFile = filepath.Join(gen, "cert.pem"), filepath.Join(gen, "key.pem")
	c.current = &cert
	c.certMod, c.keyMod = certInfo.ModTime(), keyInfo.ModTime()
	c.checked = time.Now()
	return nil
}
func (c *Certificate) Regenerate(names []string) error {
	if len(names) == 0 {
		return errors.New("at least one certificate name is required")
	}
	for _, name := range names {
		if name == "" || strings.ContainsAny(name, " /\\\t\r\n") || (net.ParseIP(name) == nil && len(name) > 253) {
			return errors.New("invalid certificate name")
		}
	}
	cert, key, err := selfSigned(names)
	if err != nil {
		return err
	}
	return c.Replace(cert, key)
}
