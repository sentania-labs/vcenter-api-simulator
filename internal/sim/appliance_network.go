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

	mac, gateway string
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
		ip, subnet, err := net.ParseCIDR(n.IPv4)
		if err != nil || ip.To4() == nil {
			return n, fmt.Errorf("appliance IPv4 must be an IPv4 address/prefix: %q", n.IPv4)
		}
		n.mac, n.gateway = localMAC(ip), defaultGateway(subnet)
		if n.mac == "" {
			b := ip.To4()
			n.mac = fmt.Sprintf("00:50:56:%02x:%02x:%02x", b[1], b[2], b[3])
		}
		if n.gateway == "" {
			first := subnet.IP.To4()
			gateway := net.IPv4(first[0], first[1], first[2], first[3]+1)
			if subnet.Contains(gateway) && !gateway.Equal(ip) {
				n.gateway = gateway.String()
			}
		}
	}
	return n, nil
}

func localMAC(ip net.IP) string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range interfaces {
		addresses, err := iface.Addrs()
		if err != nil || len(iface.HardwareAddr) == 0 {
			continue
		}
		for _, address := range addresses {
			if local, _, err := net.ParseCIDR(address.String()); err == nil && local.Equal(ip) {
				return iface.HardwareAddr.String()
			}
		}
	}
	return ""
}

func defaultGateway(subnet *net.IPNet) string {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 || fields[1] != "00000000" {
			continue
		}
		var raw uint32
		if _, err := fmt.Sscanf(fields[2], "%X", &raw); err != nil {
			continue
		}
		gateway := net.IPv4(byte(raw), byte(raw>>8), byte(raw>>16), byte(raw>>24))
		if subnet.Contains(gateway) {
			return gateway.String()
		}
	}
	return ""
}
