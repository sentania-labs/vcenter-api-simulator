package signon

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
)

// The key is made once, kept in the data directory, and used again after a
// restart for as long as the seed is the same. A new seed gets a new key.
func TestKeyIsStableForASeedAndNewForAnother(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir, "seed-a").Material()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, File))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the key file is %v, want readable by its owner only", info.Mode().Perm())
	}
	// a restart: another issuer on the same directory and seed
	again, err := Open(dir, "seed-a").Material()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Leaf, again.Leaf) || !bytes.Equal(first.Root, again.Root) || !first.Key.Equal(again.Key) {
		t.Error("the key or its certificates changed across a restart under the same seed")
	}
	other, err := Open(dir, "seed-b").Material()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.Leaf, other.Leaf) || first.Key.Equal(other.Key) {
		t.Error("another seed got the same key")
	}
	// and the new one is what the directory now holds
	kept, err := Open(dir, "seed-b").Material()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(other.Leaf, kept.Leaf) {
		t.Error("the key made for the new seed was not kept")
	}
	// nothing but the one file is left in the directory
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != File {
		t.Errorf("the directory holds %v, want only %s", entries, File)
	}
}

// The signing certificate is an RSA 3072 certificate issued by a root that
// signed itself, and the key belongs to it.
func TestMaterialIsASigningCertificateUnderARoot(t *testing.T) {
	m, err := Open("", "shape").Material()
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(m.Leaf)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(m.Root)
	if err != nil {
		t.Fatal(err)
	}
	pub, ok := leaf.PublicKey.(*rsa.PublicKey)
	if !ok || pub.N.BitLen() != 3072 || !pub.Equal(&m.Key.PublicKey) {
		t.Errorf("the certificate's key is %T, want the RSA 3072 signing key", leaf.PublicKey)
	}
	if leaf.SignatureAlgorithm != x509.SHA256WithRSA || leaf.IsCA || leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Errorf("signing certificate: algorithm %v, CA %v, usage %v", leaf.SignatureAlgorithm, leaf.IsCA, leaf.KeyUsage)
	}
	if !root.IsCA || !bytes.Equal(root.RawIssuer, root.RawSubject) {
		t.Error("the root is not a self-signed authority")
	}
	pool := x509.NewCertPool()
	pool.AddCert(root)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		t.Errorf("the signing certificate does not chain to the root: %v", err)
	}
}

// Without a directory every issuer of a seed in the process is the same
// one, so two simulators built from a seed sign alike.
func TestWithoutADirectoryASeedSharesItsKey(t *testing.T) {
	a, err := Open("", "shared").Material()
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open("", "shared").Material()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Leaf, b.Leaf) {
		t.Error("two issuers of one seed have different keys")
	}
	c, err := Open("", "not-shared").Material()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a.Leaf, c.Leaf) {
		t.Error("two seeds share a key")
	}
}

// A key file that cannot be read is an error that says what to do, never a
// silent new key under the same seed.
func TestBrokenKeyFileIsReported(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir, "seed").Material(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, File)
	for name, content := range map[string]string{
		"not JSON":        "{",
		"no key":          `{"seed":"seed","key":"","certificate":"","root":""}`,
		"key of no shape": `{"seed":"seed","key":"-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n","certificate":"","root":""}`,
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir, "seed").Material(); err == nil {
			t.Errorf("%s: no error", name)
		}
		if got, _ := os.ReadFile(path); string(got) != content {
			t.Errorf("%s: the file was replaced", name)
		}
	}
}
