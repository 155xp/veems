package main

import (
	"fmt"
	"os/exec"
	"strings"
)

// dnsForward is the address pasta answers DNS on; it relays to the host's resolver.
const dnsForward = "169.254.53.53"

// Ranges a VM must never reach: the host, the LAN, link-local, and multicast.
var blockedV4 = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15",
	"224.0.0.0/4", "240.0.0.0/4",
}

// firewallRules builds the egress firewall for a networked VM. pasta copies
// the host's addresses and routes into its namespace, so they are read here.
// If they cannot be read, the VM does not start: the fixed ranges alone miss
// a LAN or a host that uses public addresses.
func firewallRules() (string, error) {
	routes, err := exec.Command("ip", "-4", "-o", "route", "show", "scope", "link").Output()
	if err != nil {
		return "", fmt.Errorf("reading the host's routes for the firewall: %v (is iproute2 installed?)", err)
	}
	addrs, err := exec.Command("ip", "-4", "-o", "addr", "show").Output()
	if err != nil {
		return "", fmt.Errorf("reading the host's addresses for the firewall: %v (is iproute2 installed?)", err)
	}
	return nftRules(hostNets(string(routes), string(addrs))), nil
}

// hostNets lists the subnets attached to the host and its own addresses, from
// `ip -o route show scope link` and `ip -o addr show`.
func hostNets(routes, addrs string) []string {
	var nets []string
	for _, line := range strings.Split(routes, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] != "default" {
			nets = append(nets, f[0])
		}
	}
	for _, line := range strings.Split(addrs, "\n") {
		f := strings.Fields(line)
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "inet" {
				nets = append(nets, strings.SplitN(f[i+1], "/", 2)[0])
			}
		}
	}
	return nets
}

func nftRules(hostNets []string) string {
	return fmt.Sprintf(`table inet veems {
	chain out {
		type filter hook output priority 0; policy accept;
		ip daddr %[1]s udp dport 53 accept
		ip daddr %[1]s tcp dport 53 accept
%[2]s		meta nfproto ipv6 reject
	}
}
`, dnsForward, blockRules(append(append([]string{}, blockedV4...), hostNets...)))
}

// netjailScript runs inside pasta's network namespace, where it holds
// CAP_NET_ADMIN over that namespace only. It loads the firewall, then execs
// the sandboxed QEMU; if the firewall fails, the VM never starts. (It is a
// shell script because pasta's AppArmor profile only lets it exec /usr/bin.)
const netjailScript = `printf '%s' "$1" | "$2" -f - || { echo "veems: firewall setup failed, refusing to start" >&2; exit 1; }; shift 2; exec "$@"`

func nftPath() string {
	if p, err := exec.LookPath("nft"); err == nil {
		return p
	}
	return "/usr/sbin/nft"
}

// One rule per prefix: nft rejects anonymous sets whose intervals overlap.
func blockRules(prefixes []string) string {
	var b strings.Builder
	seen := map[string]bool{}
	for _, p := range prefixes {
		if !seen[p] {
			seen[p] = true
			fmt.Fprintf(&b, "\t\tip daddr %s reject with icmp type admin-prohibited\n", p)
		}
	}
	return b.String()
}
