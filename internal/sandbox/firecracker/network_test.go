package firecracker

import (
	"net/netip"
	"strings"
	"testing"
)

func TestNetworkLinks(t *testing.T) {
	c := NetworkConfig{Subnet: netip.MustParsePrefix("172.31.0.0/16"), ProxyPort: 3128}
	if err := c.Validate(2); err != nil {
		t.Fatal(err)
	}
	l0, l1 := c.Link(0), c.Link(1)
	if l0.Tap != "hcc0" || l0.Host.String() != "172.31.0.1" || l0.Guest.String() != "172.31.0.2/30" {
		t.Errorf("slot 0 = %+v", l0)
	}
	if l1.Tap != "hcc1" || l1.Host.String() != "172.31.0.5" || l1.Guest.String() != "172.31.0.6/30" {
		t.Errorf("slot 1 = %+v", l1)
	}
	if last := c.Link(MaxIdentities - 1); last.Tap != "hcc1023" || last.Guest.String() != "172.31.15.254/30" || last.Valid() != nil {
		t.Errorf("last slot = %+v", last)
	}
	for _, l := range []NetInfo{l0, l1} {
		if err := l.Valid(); err != nil {
			t.Error(err)
		}
	}
}

func TestNetworkValidate(t *testing.T) {
	bad := map[string]NetworkConfig{
		"ipv6":       {Subnet: netip.MustParsePrefix("fd00::/64"), ProxyPort: 3128},
		"too small":  {Subnet: netip.MustParsePrefix("172.31.0.0/31"), ProxyPort: 3128},
		"not masked": {Subnet: netip.MustParsePrefix("172.31.0.1/16"), ProxyPort: 3128},
		"no room":    {Subnet: netip.MustParsePrefix("172.31.0.0/29"), ProxyPort: 3128},
		"priv. port": {Subnet: netip.MustParsePrefix("172.31.0.0/16"), ProxyPort: 80},
		"no subnet":  {ProxyPort: 3128},
	}
	for name, c := range bad {
		if c.Validate(4) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNetInfoValid(t *testing.T) {
	good := NetInfo{Tap: "hcc0", Host: netip.MustParseAddr("172.31.0.1"), Guest: netip.MustParsePrefix("172.31.0.2/30"), ProxyPort: 3128}
	bad := map[string]func(*NetInfo){
		"tap name":     func(n *NetInfo) { n.Tap = "eth0" },
		"tap inject":   func(n *NetInfo) { n.Tap = "hcc0;reboot" },
		"other subnet": func(n *NetInfo) { n.Guest = netip.MustParsePrefix("10.0.0.2/30") },
		"wide guest":   func(n *NetInfo) { n.Guest = netip.MustParsePrefix("172.31.0.2/16") },
		"ipv6 host":    func(n *NetInfo) { n.Host = netip.MustParseAddr("::1") },
		"low port":     func(n *NetInfo) { n.ProxyPort = 22 },
	}
	if good.Valid() != nil {
		t.Fatal(good.Valid())
	}
	for name, mutate := range bad {
		n := good
		mutate(&n)
		if n.Valid() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRuleset(t *testing.T) {
	c := NetworkConfig{Subnet: netip.MustParsePrefix("172.31.0.0/16"), ProxyPort: 3128}
	r := c.Ruleset()
	for _, want := range []string{
		"delete table inet hcc_guard",
		`iifname "hcc*" tcp dport 3128 fib daddr . iif type local ct state new accept`,
		`iifname "hcc*" counter drop`,
		`oifname "hcc*" counter drop`,
	} {
		if !strings.Contains(r, want) {
			t.Errorf("ruleset missing %q", want)
		}
	}
}
