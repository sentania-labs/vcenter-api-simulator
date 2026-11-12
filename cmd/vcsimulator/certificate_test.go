package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/serve"
)

func TestManagedCertificateOverridesAndRestoresConsoleCertificate(t *testing.T) {
	dir := t.TempDir()
	sc := scenario.Phase1("cert-test")
	console, err := certificate(sc, dir, "", "", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err = console.Regenerate([]string{"console.example.test"}); err != nil {
		t.Fatal(err)
	}
	saved := console.Info()
	externalDir := filepath.Join(dir, "deployment-tls")
	external, err := serve.LoadOrCreateCertificate(externalDir, []string{"deployment.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	managed, err := certificate(sc, dir, externalDir, "", "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if got := managed.Info(); !got.Managed || got.Fingerprint != external.Info().Fingerprint || got.Fingerprint == saved.Fingerprint {
		t.Fatal("deployment certificate did not take priority")
	}
	if err = managed.Regenerate([]string{"new.example.test"}); !errors.Is(err, serve.ErrCertificateManaged) {
		t.Fatalf("managed change: %v", err)
	}
	fromFlags, err := certificate(sc, dir, "", filepath.Join(externalDir, "cert.pem"), filepath.Join(externalDir, "key.pem"), "", false)
	if err != nil || !fromFlags.Info().Managed || fromFlags.Info().Fingerprint != external.Info().Fingerprint {
		t.Fatal("explicit certificate did not take priority")
	}
	sc.TLS.CertFile, sc.TLS.KeyFile = filepath.Join(externalDir, "cert.pem"), filepath.Join(externalDir, "key.pem")
	fromScenario, err := certificate(sc, dir, "", "", "", "", false)
	if err != nil || !fromScenario.Info().Managed || fromScenario.Info().Fingerprint != external.Info().Fingerprint {
		t.Fatal("scenario certificate did not take priority")
	}
	sc.TLS = scenario.TLS{}
	restored, err := certificate(sc, dir, "", "", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Info().Fingerprint != saved.Fingerprint {
		t.Fatal("console certificate was not restored")
	}
}
