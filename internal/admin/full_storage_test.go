package admin

import (
	"testing"

	"golang.org/x/crypto/bcrypt"
	"os"
	"path/filepath"
)

func TestDeploymentPasswordSurvivesFullVolume(t *testing.T) {
	if _, err := os.Stat("/dev/full"); err != nil {
		t.Skip("requires /dev/full")
	}
	dir := t.TempDir()
	if err := os.Symlink("/dev/full", filepath.Join(dir, "console-password")); err != nil {
		t.Fatal(err)
	}
	// Exercise the ENOSPC handling without reading the unbounded /dev/full.
	a := &consoleAuth{}
	err := os.WriteFile(filepath.Join(dir, "console-password"), []byte("test"), 0600)
	if err == nil {
		t.Fatal("expected ENOSPC")
	}
	if err = a.unsavedSeed("test-deployment-password", err); err != nil {
		t.Fatal(err)
	}
	if ok, err := a.claimed(); err != nil || !ok {
		t.Fatalf("claimed=%v err=%v", ok, err)
	}
	hash, err := a.readHash()
	if err != nil || bcrypt.CompareHashAndPassword(hash, []byte("test-deployment-password")) != nil {
		t.Fatal("password not available")
	}
}
