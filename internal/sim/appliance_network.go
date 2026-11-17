package sim

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
)

// ApplianceNetwork configures the appliance's reported networking. Empty values
// use this process's resolver and first non-loopback IPv4 interface. IPv4 is an
// address/prefix, not an ingress VIP inferred from a DNS lookup.
type ApplianceNetwork struct {
	DNSServers []string
	IPv4       string
}

func (n ApplianceNetwork) resolve() (ApplianceNetwork, error) {
	n.DNSServers = append([]string{}, n.DNSServers...)
	if len(n.DNSServers) == 0 {
		if f, err := os.Open("/etc/resolv.conf"); err == nil {
			defer f.Close()
			scanner := bufio.NewScanner(f)
			for scanner.Scan() {
				fields := strings.Fields(scanner.Text())
				if len(fields) > 1 && fields[0] == "nameserver" && net.ParseIP(fields[1]) != nil {
					n.DNSServers = append(n.DNSServers, fields[1])
				}
			}
		}
	}
	for _, server := range n.DNSServers {
		if net.ParseIP(server) == nil {
			return n, fmt.Errorf("appliance DNS server must be an IP address: %q", server)
		}
	}
	if n.IPv4 == "" {
		if addresses, err := net.InterfaceAddrs(); err == nil {
			for _, address := range addresses {
				if ip, _, err := net.ParseCIDR(address.String()); err == nil && ip.To4() != nil && !ip.IsLoopback() {
					n.IPv4 = address.String()
					break
				}
			}
		}
	}
	if n.IPv4 != "" {
		if ip, _, err := net.ParseCIDR(n.IPv4); err != nil || ip.To4() == nil {
			return n, fmt.Errorf("appliance IPv4 must be an IPv4 address/prefix: %q", n.IPv4)
		}
	}
	return n, nil
}
