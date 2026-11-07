// Command vcsimulator serves a simulated vCenter 9.1.1 with vSAN ESA
// clusters on /sdk and /vsanHealth, for the vSAN host metrics management
// pack and the native vCenter adapter.
//
//	vcsimulator -scenario examples/scenarios/ten-clusters.yaml -data ./data -listen 0.0.0.0:8443
//
// The scenario file (package scenario) describes the inventory, who may log
// in, the certificate and the failure and latency knobs. The data directory
// holds the identity registry (registry.json) and, unless the scenario names
// a certificate, the self-signed key pair (tls/cert.pem, tls/key.pem); both
// persist across restarts so identities and the pinned certificate do not
// change. Without -scenario the built-in Phase 1 scenario (one four-host
// cluster) runs under -seed.
//
// Login accepts any user name and password (vcsim's default) unless the
// scenario configures users or -username is set (or VCSIM_USERNAME /
// VCSIM_PASSWORD in the environment), in which case only those users log in.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/serve"
	"github.com/sentania/vcenter-api-simulator/internal/sim"
	"github.com/sentania/vcenter-api-simulator/internal/topology"
)

// version and commit are stamped by the release build (-ldflags -X).
var (
	version = "dev"
	commit  = "unknown"
)

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	var listen multi
	flag.Var(&listen, "listen", "address to serve HTTPS on (repeatable; default 0.0.0.0:8443)")
	scenarioPath := flag.String("scenario", "", "scenario file (YAML); default: the built-in one-cluster scenario")
	dataDir := flag.String("data", "data", "directory for the identity registry and the self-signed certificate")
	seed := flag.String("seed", "phase1", "identity seed for the built-in scenario (ignored with -scenario)")
	regenerate := flag.Bool("regenerate", false, "discard the identity registry (required after a seed change; ids change only if the seed did)")
	tlsDir := flag.String("tls-dir", "", "directory holding the persisted self-signed certificate (default <data>/tls)")
	tlsCert := flag.String("tls-cert", "", "serve this certificate (PEM) instead of a self-signed one; needs -tls-key")
	tlsKey := flag.String("tls-key", "", "private key (PEM) for -tls-cert")
	names := flag.String("tls-names", "", "comma-separated extra DNS names or IPs for a newly created certificate")
	username := flag.String("username", "", "only user name Login accepts; empty accepts any (env VCSIM_USERNAME)")
	password := flag.String("password", "", "password for -username (env VCSIM_PASSWORD)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	sessionTimeout := flag.Duration("session-timeout", 0, "idle session timeout (0: sessions never expire)")
	flag.Parse()
	if *username == "" {
		*username = os.Getenv("VCSIM_USERNAME")
	}
	if *password == "" {
		*password = os.Getenv("VCSIM_PASSWORD")
	}
	if *showVersion {
		fmt.Printf("vcsimulator %s (%s)\n", version, commit)
		return
	}
	if len(listen) == 0 {
		listen = multi{"0.0.0.0:8443"}
	}

	var sc *scenario.Scenario
	var err error
	if *scenarioPath != "" {
		if sc, err = scenario.Load(*scenarioPath); err != nil {
			log.Fatal(err)
		}
	} else {
		sc = scenario.Phase1(*seed)
	}

	reg, err := registry.Open(filepath.Join(*dataDir, "registry.json"), sc.Seed, *regenerate)
	if err != nil {
		log.Fatal(err)
	}
	topo := topology.Generate(sc, reg)
	if err := reg.Save(); err != nil {
		log.Fatal(err)
	}

	cert, err := certificate(sc, *dataDir, *tlsDir, *tlsCert, *tlsKey, *names)
	if err != nil {
		log.Fatal(err)
	}
	creds, err := sc.Credentials()
	if err != nil {
		log.Fatal(err)
	}

	start := time.Now()
	if *username != "" {
		// the flag/env user joins the scenario's users
		if creds == nil {
			creds = map[string]string{}
		}
		creds[*username] = *password
	}
	s, err := sim.New(sim.Options{
		Topology:       topo,
		Credentials:    creds,
		SessionTimeout: *sessionTimeout,
		Faults:         sc.Faults,
		Latency:        sc.Latency,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()

	ls, err := serve.Listen(listen, s.Handler, cert)
	if err != nil {
		log.Fatal(err)
	}
	defer ls.Close()

	fmt.Printf("vcsimulator: %s (%s)\n", version, commit)
	clusters, hosts, vms, devices := topo.Counts()
	fmt.Printf("vcsimulator: vCenter %s instanceUuid=%s seed=%q registry=%d ids built in %s\n",
		s.Catalog.About.Version, topo.InstanceUUID, sc.Seed, reg.Len(), time.Since(start).Round(time.Millisecond))
	fmt.Printf("vcsimulator: %d clusters, %d hosts, %d vms, %d devices\n", clusters, hosts, vms, devices)
	fmt.Printf("vcsimulator: %s\n", loginMode(creds))
	for _, c := range s.Clusters {
		cl := c.Cluster
		fmt.Printf("vcsimulator: %s (%s) vsan uuid=%s hosts=%d vms=%d dedup=%v\n", cl.Key, c.Ref.Value, cl.VsanUUID, len(cl.Hosts), len(cl.VMs), cl.Dedup)
	}
	for _, a := range ls.Addrs {
		fmt.Printf("vcsimulator: listening on https://%s/sdk and /vsanHealth\n", a)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
}

func loginMode(creds map[string]string) string {
	if len(creds) == 0 {
		return "login accepts any user name and password"
	}
	return fmt.Sprintf("login restricted to %d user(s)", len(creds))
}

// certificate picks the key pair: flags over the scenario, a provided pair
// over a persisted self-signed one.
func certificate(sc *scenario.Scenario, dataDir, tlsDir, certFile, keyFile, names string) (*serve.Certificate, error) {
	if certFile == "" {
		certFile, keyFile = sc.TLS.CertFile, sc.TLS.KeyFile
	}
	if certFile != "" {
		return serve.LoadCertificateFiles(certFile, keyFile)
	}
	if tlsDir == "" {
		tlsDir = sc.TLS.Dir
	}
	if tlsDir == "" {
		tlsDir = filepath.Join(dataDir, "tls")
	}
	san := []string{"localhost", "127.0.0.1"}
	if h, err := os.Hostname(); err == nil {
		san = append(san, h)
	}
	san = append(san, sc.TLS.Names...)
	for _, n := range strings.Split(names, ",") {
		if n = strings.TrimSpace(n); n != "" {
			san = append(san, n)
		}
	}
	return serve.LoadOrCreateCertificate(tlsDir, san)
}
