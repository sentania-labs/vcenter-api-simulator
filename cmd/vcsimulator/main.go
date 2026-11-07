// Command vcsimulator serves a simulated vCenter 9.1.1 with one vSAN ESA
// cluster on /sdk and /vsanHealth, for the vSAN host metrics management pack.
//
//	vcsimulator -listen 0.0.0.0:8443 -listen 0.0.0.0:443 -tls-dir ./data/tls -seed phase1
//
// Login accepts any user name and password (vcsim's default) unless
// -username is set (or VCSIM_USERNAME / VCSIM_PASSWORD in the environment),
// in which case only that pair logs in.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

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
	tlsDir := flag.String("tls-dir", "data/tls", "directory holding the persisted certificate (cert.pem, key.pem)")
	seed := flag.String("seed", "phase1", "identity seed; the same seed reproduces every MoRef, UUID and metric value")
	names := flag.String("tls-names", "", "comma-separated extra DNS names or IPs for a newly created certificate")
	username := flag.String("username", "", "only user name Login accepts; empty accepts any (env VCSIM_USERNAME)")
	password := flag.String("password", "", "password for -username (env VCSIM_PASSWORD)")
	showVersion := flag.Bool("version", false, "print the version and exit")
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

	san := []string{"localhost", "127.0.0.1"}
	if h, err := os.Hostname(); err == nil {
		san = append(san, h)
	}
	for _, n := range strings.Split(*names, ",") {
		if n = strings.TrimSpace(n); n != "" {
			san = append(san, n)
		}
	}
	cert, err := serve.LoadOrCreateCertificate(*tlsDir, san)
	if err != nil {
		log.Fatal(err)
	}

	topo := topology.Phase1(*seed)
	s, err := sim.New(sim.Options{Topology: topo, Username: *username, Password: *password})
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
	fmt.Printf("vcsimulator: vCenter %s instanceUuid=%s seed=%q\n", s.Catalog.About.Version, topo.InstanceUUID, *seed)
	fmt.Printf("vcsimulator: cluster %s (%s) vsan uuid=%s hosts=%d vms=%d\n", topo.Cluster.Name, s.ClusterRef.Value, topo.Cluster.VsanUUID, len(topo.Cluster.Hosts), len(topo.Cluster.VMs))
	if *username != "" {
		fmt.Printf("vcsimulator: login restricted to user %q\n", *username)
	} else {
		fmt.Println("vcsimulator: login accepts any user name and password")
	}
	for _, a := range ls.Addrs {
		fmt.Printf("vcsimulator: listening on https://%s/sdk and /vsanHealth\n", a)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
}
