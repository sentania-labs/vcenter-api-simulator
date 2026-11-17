// Package signon holds the key the simulated sign-on service signs its
// tokens with, and the two certificates that go with it: the signing
// certificate and the self-signed root that issued it.
//
// The key is the simulator's own. It is made the first time it is needed,
// kept in the data directory beside the identity registry, and reused for as
// long as the seed stays the same, so a tool that fetched the signing
// certificate before a restart still verifies tokens issued after it. A new
// seed is a new simulated vCenter and gets a new key.
package signon

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/storage"
)

// SystemDomain is the sign-on domain of the simulated vCenter. It is the
// last part of both sign-on addresses and the domain of every group a token
// lists.
const SystemDomain = "vsphere.local"

// File is the name of the key file in the data directory.
const File = "sign-on-key.json"

const (
	signingBits = 3072 // the size a real vCenter's token-signing key showed
	rootBits    = 2048
)

// Material is the signing key with its certificates, both DER.
type Material struct {
	Key  *rsa.PrivateKey
	Leaf []byte // the signing certificate: what GetIssuersCertificates returns
	Root []byte // the self-signed root that issued Leaf
}

// Issuer makes or loads the material on first use.
type Issuer struct {
	dir, seed string
	once      sync.Once
	material  *Material
	err       error
}

// memory holds the issuers of simulators that have no data directory, one
// per seed, so that every simulator built from a seed in one process signs
// with the same key.
var memory sync.Map

// Open returns the issuer of a seed. With a directory the key is kept in
// it; without one it lives as long as the process.
func Open(dir, seed string) *Issuer {
	if dir == "" {
		issuer, _ := memory.LoadOrStore(seed, &Issuer{seed: seed})
		return issuer.(*Issuer)
	}
	return &Issuer{dir: dir, seed: seed}
}

// Material returns the key and certificates, making them if this is the
// first use under this seed.
func (i *Issuer) Material() (*Material, error) {
	i.once.Do(func() { i.material, i.err = i.load() })
	return i.material, i.err
}

type saved struct {
	Seed        string `json:"seed"`
	Key         string `json:"key"`
	Certificate string `json:"certificate"`
	Root        string `json:"root"`
}

func (i *Issuer) load() (*Material, error) {
	if i.dir == "" {
		return generate()
	}
	path := filepath.Join(i.dir, File)
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		var s saved
		if err := json.Unmarshal(b, &s); err != nil {
			return nil, fmt.Errorf("signon: %s: %w (delete it to make a new key)", path, err)
		}
		if s.Seed == i.seed {
			m, err := decode(s)
			if err != nil {
				return nil, fmt.Errorf("signon: %s: %w (delete it to make a new key)", path, err)
			}
			return m, nil
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	m, err := generate()
	if err != nil {
		return nil, err
	}
	if err := i.save(path, m); err != nil {
		if !storage.Full(err) {
			return nil, err
		}
		log.Printf("signon: data volume full; serving with an unsaved signing key; free space or enlarge the volume: %v", err)
	}
	return m, nil
}

func decode(s saved) (*Material, error) {
	block := func(text, kind string) ([]byte, error) {
		b, _ := pem.Decode([]byte(text))
		if b == nil || b.Type != kind {
			return nil, fmt.Errorf("no %s", kind)
		}
		return b.Bytes, nil
	}
	keyDER, err := block(s.Key, "PRIVATE KEY")
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParsePKCS8PrivateKey(keyDER)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("the key is not an RSA key")
	}
	leaf, err := block(s.Certificate, "CERTIFICATE")
	if err != nil {
		return nil, err
	}
	root, err := block(s.Root, "CERTIFICATE")
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(leaf)
	if err != nil {
		return nil, err
	}
	if pub, ok := cert.PublicKey.(*rsa.PublicKey); !ok || !pub.Equal(&key.PublicKey) {
		return nil, errors.New("the certificate does not belong to the key")
	}
	return &Material{Key: key, Leaf: leaf, Root: root}, nil
}

func (i *Issuer) save(path string, m *Material) error {
	keyDER, err := x509.MarshalPKCS8PrivateKey(m.Key)
	if err != nil {
		return err
	}
	text := func(kind string, der []byte) string {
		return string(pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}))
	}
	b, err := json.Marshal(saved{Seed: i.seed, Key: text("PRIVATE KEY", keyDER), Certificate: text("CERTIFICATE", m.Leaf), Root: text("CERTIFICATE", m.Root)})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(i.dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(i.dir, ".sign-on-key-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// generate makes a root, a signing key and the signing certificate the root
// issues. The root's key is used once and not kept: nothing else is ever
// issued under it.
func generate() (*Material, error) {
	rootKey, err := rsa.GenerateKey(rand.Reader, rootBits)
	if err != nil {
		return nil, err
	}
	key, err := rsa.GenerateKey(rand.Reader, signingBits)
	if err != nil {
		return nil, err
	}
	serial := func() (*big.Int, error) { return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127)) }
	rootSerial, err := serial()
	if err != nil {
		return nil, err
	}
	leafSerial, err := serial()
	if err != nil {
		return nil, err
	}
	// a day's allowance for a tool whose clock runs behind the simulator's
	from, until := time.Now().Add(-24*time.Hour), time.Now().AddDate(10, 0, 0)
	rootTmpl := &x509.Certificate{
		SerialNumber:          rootSerial,
		Subject:               pkix.Name{CommonName: "vcenter-api-simulator sign-on root", Organization: []string{"vcenter-api-simulator"}},
		NotBefore:             from,
		NotAfter:              until,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		SignatureAlgorithm:    x509.SHA256WithRSA,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey)
	if err != nil {
		return nil, err
	}
	rootCert, err := x509.ParseCertificate(rootDER)
	if err != nil {
		return nil, err
	}
	leafTmpl := &x509.Certificate{
		SerialNumber:          leafSerial,
		Subject:               pkix.Name{CommonName: "vcenter-api-simulator sign-on token signing", Organization: []string{"vcenter-api-simulator"}},
		NotBefore:             from,
		NotAfter:              until,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		SignatureAlgorithm:    x509.SHA256WithRSA,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, rootCert, &key.PublicKey, rootKey)
	if err != nil {
		return nil, err
	}
	return &Material{Key: key, Leaf: leafDER, Root: rootDER}, nil
}
