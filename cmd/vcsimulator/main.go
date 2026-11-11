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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sentania/vcenter-api-simulator/internal/admin"
	"github.com/sentania/vcenter-api-simulator/internal/catalog"
	"github.com/sentania/vcenter-api-simulator/internal/control"
	"github.com/sentania/vcenter-api-simulator/internal/querylog"
	"github.com/sentania/vcenter-api-simulator/internal/registry"
	"github.com/sentania/vcenter-api-simulator/internal/scenario"
	"github.com/sentania/vcenter-api-simulator/internal/serve"
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
	dataDir := flag.String("data", "", "directory for the identity registry and the self-signed certificate (env VCSIM_DATA; default data)")
	seed := flag.String("seed", "phase1", "identity seed for the built-in scenario (ignored with -scenario)")
	queryLog := flag.String("query-log", "", "enable full query capture to this JSONL path (may contain inventory identifiers)")
	unansweredStdout := flag.Bool("unanswered-stdout", false, "write unanswered requests as JSON lines to stdout")
	regenerate := flag.Bool("regenerate", false, "discard the identity registry (required after a seed change; ids change only if the seed did)")
	tlsManaged := flag.Bool("tls-managed", false, "the deployment manages the certificate pair in -tls-dir")
	tlsDir := flag.String("tls-dir", "", "directory holding the persisted self-signed certificate (default <data>/tls)")
	tlsCert := flag.String("tls-cert", "", "serve this certificate (PEM) instead of a self-signed one; needs -tls-key")
	tlsKey := flag.String("tls-key", "", "private key (PEM) for -tls-cert")
	names := flag.String("tls-names", "", "comma-separated extra DNS names or IPs for a newly created certificate")
	anyLogin := flag.Bool("any-login", false, "accept any nonempty vCenter user name and password")
	username := flag.String("username", "", "additional accepted user name (env VCSIM_USERNAME)")
	password := flag.String("password", "", "password for -username (env VCSIM_PASSWORD)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	describe := flag.Bool("describe", false, "print the scenario's cardinality as JSON (clusters, hosts, VMs, devices, per cluster) and exit without serving; needs no data directory")
	adminAddr := flag.String("admin", "127.0.0.1:9090", "address for the control console (plain HTTP; keep on loopback or behind TLS; empty disables; env VCSIM_ADMIN)")
	adminSecure := flag.Bool("admin-secure-cookie", false, "mark console session cookies Secure behind a TLS-terminating proxy")
	adminReset := flag.Bool("admin-reset", false, "remove the console password and exit; stop the console first")
	adminHosts := flag.String("admin-allowed-hosts", "", "comma-separated Host names the control console answers to besides localhost, 127.0.0.1 and ::1; any other Host gets 403 (env VCSIM_ADMIN_ALLOWED_HOSTS)")
	sessionTimeout := flag.Duration("session-timeout", 0, "idle session timeout (0: sessions never expire)")
	flag.Parse()
	adminSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "admin" {
			adminSet = true
		}
	})
	if v, ok := os.LookupEnv("VCSIM_ADMIN"); ok && !adminSet {
		*adminAddr = v
	}
	if *adminHosts == "" {
		*adminHosts = os.Getenv("VCSIM_ADMIN_ALLOWED_HOSTS")
	}
	if *username == "" {
		*username = os.Getenv("VCSIM_USERNAME")
	}
	if *password == "" {
		*password = os.Getenv("VCSIM_PASSWORD")
	}
	if *dataDir == "" {
		// the container image sets VCSIM_DATA to its state volume so a
		// custom command line without -data still lands on writable storage
		*dataDir = os.Getenv("VCSIM_DATA")
	}
	if *dataDir == "" {
		*dataDir = "data"
	}
	if *adminReset {
		if err := os.Remove(filepath.Join(*dataDir, "console-password")); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Fatal(err)
		}
		fmt.Println("console password removed; the next visitor can claim the console")
		return
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

	if *describe {
		// the cardinality the scenario materializes, for scripts that need
		// the expected counts (the acceptance harness, projections)
		if err := json.NewEncoder(os.Stdout).Encode(describeTopology(topology.Generate(sc, registry.InMemory(sc.Seed)))); err != nil {
			log.Fatal(err)
		}
		return
	}
	cert, err := certificate(sc, *dataDir, *tlsDir, *tlsCert, *tlsKey, *names, *tlsManaged)
	if err != nil {
		log.Fatal(err)
	}
	if _, err := sc.Credentials(); err != nil {
		log.Fatal(err)
	}
	extra := map[string]string{}
	if *username != "" {
		// the flag/env user joins the scenario's users
		extra[*username] = *password
	}

	start := time.Now()
	ctl, err := control.New(control.Options{
		Scenario: sc, SourceHash: sourceHash(*scenarioPath, sc), DataDir: *dataDir, Regenerate: *regenerate,
		Credentials: extra, SessionTimeout: *sessionTimeout, AnyLogin: *anyLogin, Certificate: cert,
		Version: version, Commit: commit, Endpoint: endpointName(*names), APIAddrs: listen, AdminAddr: *adminAddr,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer ctl.Close()

	capture, err := querylog.NewCapture(*dataDir, *queryLog, querylog.Settings{Full: *queryLog != "", Unanswered: true, Stdout: *unansweredStdout}, os.Stdout)
	if err != nil {
		log.Fatal(err)
	}
	var handler http.Handler = capture.Wrap(ctl)
	ls, err := serve.Listen(listen, handler, cert)
	if err != nil {
		log.Fatal(err)
	}
	defer ls.Close()

	var adminSrv *http.Server
	if *adminAddr != "" {
		ln, err := net.Listen("tcp", *adminAddr)
		switch {
		case err != nil && adminSet:
			log.Fatal(err)
		case err != nil:
			// the default address is taken (another instance on this host):
			// the API still serves, the console is off
			fmt.Printf("vcsimulator: control console not started: %v (set -admin to another address)\n", err)
			*adminAddr = ""
		default:
			adminHandler, err := admin.ProtectedHandler(ctl, strings.Split(*adminHosts, ","), *dataDir, os.Getenv("VCSIM_ADMIN_PASSWORD"), *adminSecure, capture)
			if err != nil {
				log.Fatal(err)
			}
			adminSrv = &http.Server{Handler: adminHandler, ReadHeaderTimeout: 30 * time.Second}
			go func() {
				if err := adminSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					fmt.Println("admin:", err)
				}
			}()
			defer adminSrv.Close()
			*adminAddr = ln.Addr().String()
		}
	}

	fmt.Printf("vcsimulator: %s (%s)\n", version, commit)
	for _, line := range ctl.Banner(time.Since(start)) {
		fmt.Println("vcsimulator: " + line)
	}
	fmt.Printf("vcsimulator: %s\n", loginMode(ctl.LoginUsers()))
	for _, a := range ls.Addrs {
		fmt.Printf("vcsimulator: listening on https://%s/sdk and /vsanHealth\n", a)
	}
	if *adminAddr != "" {
		fmt.Printf("vcsimulator: control console on http://%s/ (login required; %s)\n", *adminAddr, adminNote(*adminAddr))
	}
	// machine-readable form of the same counts, for the benchmark harness
	desc := describeTopology(topology.Generate(sc, registry.InMemory(sc.Seed)))
	for _, a := range ls.Addrs {
		fmt.Printf("VCSIM_JSON %s\n", jsonLine(desc, a))
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
}

// sourceHash identifies the scenario the process started from, so a saved
// console state is dropped when the operator edits the file.
func sourceHash(path string, sc *scenario.Scenario) string {
	if path == "" {
		return "builtin:" + sc.Seed
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "file:" + path
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// endpointName is the host name shown as the API endpoint: the first
// extra certificate name, else the machine's.
func endpointName(names string) string {
	for _, n := range strings.Split(names, ",") {
		if n = strings.TrimSpace(n); n != "" {
			return n
		}
	}
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "localhost"
}

func adminNote(addr string) string {
	host, _, _ := net.SplitHostPort(addr)
	if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
		return "loopback only"
	}
	return "reachable beyond this host: put it behind your own authentication"
}

func loginMode(users []string) string {
	if len(users) == 0 {
		return "login accepts any user name and password"
	}
	return fmt.Sprintf("login restricted to %d user(s)", len(users))
}

// certificate picks the key pair: flags over the scenario, a provided pair
// over a persisted self-signed one.
func certificate(sc *scenario.Scenario, dataDir, tlsDir, certFile, keyFile, names string, managed bool) (*serve.Certificate, error) {
	if certFile != "" {
		cert, err := serve.LoadCertificateFiles(certFile, keyFile)
		if err == nil {
			cert.MarkManaged()
		}
		return cert, err
	}
	if tlsDir == "" {
		tlsDir = sc.TLS.Dir
	}
	if tlsDir == "" {
		tlsDir = filepath.Join(dataDir, "tls")
	}
	if managed {
		cert, err := serve.LoadCertificateFiles(filepath.Join(tlsDir, "cert.pem"), filepath.Join(tlsDir, "key.pem"))
		if err == nil {
			cert.MarkManaged()
		}
		return cert, err
	}
	if sc.TLS.CertFile != "" {
		cert, err := serve.LoadCertificateFiles(sc.TLS.CertFile, sc.TLS.KeyFile)
		if err == nil {
			cert.MarkManaged()
		}
		return cert, err
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
	cert, err := serve.LoadOrCreateCertificate(tlsDir, san)
	if err != nil {
		return nil, err
	}
	if err := cert.EnableConsole(dataDir); err != nil {
		return nil, err
	}
	return cert, nil
}

// jsonLine renders a description with the listen address.
func jsonLine(d description, addr string) string {
	d.Addr = addr
	b, err := json.Marshal(d)
	if err != nil {
		log.Fatal(err)
	}
	return string(b)
}

// description is what -describe and the startup line carry.
type description struct {
	Addr       string               `json:"addr,omitempty"`
	Seed       string               `json:"seed"`
	Clusters   int                  `json:"clusters"`
	Hosts      int                  `json:"hosts"`
	VMs        int                  `json:"vms"`
	Devices    int                  `json:"devices"`
	Per        []clusterDescription `json:"perCluster"`
	Projection *control.Projection  `json:"projection"`
}

type clusterDescription struct {
	Key            string `json:"key"`
	Hosts          int    `json:"hosts"`
	VMs            int    `json:"vms"`
	ClaimedDevices int    `json:"claimedDevices"`
	Threads        int    `json:"threads"`
	DomWorlds      int    `json:"domWorlds"`
	VsanPnics      int    `json:"vsanPnics"`
	Dedup          bool   `json:"dedup"`
}

func describeTopology(topo *topology.Topology) description {
	d := description{Seed: topo.Seed, Projection: control.Project(catalog.MustLoad(), topo)}
	d.Clusters, d.Hosts, d.VMs, d.Devices = topo.Counts()
	for _, c := range topo.Clusters() {
		claimed := 0
		for _, dev := range c.Hosts[0].Devices {
			if dev.Claimed {
				claimed++
			}
		}
		d.Per = append(d.Per, clusterDescription{
			Key: c.Key, Hosts: len(c.Hosts), VMs: len(c.VMs), ClaimedDevices: claimed,
			Threads: c.Spec.Host.Threads, DomWorlds: scenario.Int(c.Spec.Host.DomWorlds),
			VsanPnics: c.Spec.Host.VsanPnics, Dedup: c.Dedup,
		})
	}
	return d
}
