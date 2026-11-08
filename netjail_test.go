package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFirewallBlocksTheHostsNetworks(t *testing.T) {
	routes := "default via 192.168.1.1 dev eth0 proto dhcp\n203.0.113.0/24 dev eth1 proto kernel scope link src 203.0.113.5\n"
	addrs := "2: eth1    inet 203.0.113.5/24 brd 203.0.113.255 scope global eth1\\       valid_lft forever\n"
	rules := nftRules(hostNets(routes, addrs))
	for _, want := range []string{
		"ip daddr 203.0.113.0/24 reject", // a LAN with public addresses
		"ip daddr 203.0.113.5 reject",    // the host itself
		"ip daddr 192.168.0.0/16 reject",
		"ip daddr 127.0.0.0/8 reject",
		"meta nfproto ipv6 reject",
		"ip daddr " + dnsForward + " udp dport 53 accept",
	} {
		if !strings.Contains(rules, want) {
			t.Errorf("rules lack %q:\n%s", want, rules)
		}
	}
	if strings.Contains(rules, "default") {
		t.Errorf("the default route was blocked:\n%s", rules)
	}
}

func TestDialQEMURefusesAnythingButASocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if c, err := dialQEMU(sock, time.Second); err != nil {
		t.Fatalf("real socket: %v", err)
	} else {
		c.Close()
	}
	link := filepath.Join(dir, "link.sock")
	if err := os.Symlink(sock, link); err != nil {
		t.Fatal(err)
	}
	if _, err := dialQEMU(link, time.Second); err == nil {
		t.Error("followed a symlink to a socket")
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := dialQEMU(file, time.Second); err == nil {
		t.Error("dialed a regular file")
	}
}
