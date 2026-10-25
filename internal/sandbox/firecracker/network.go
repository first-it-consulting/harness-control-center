package firecracker

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
)

// NetworkConfig is the root-side network setup for sandboxes: every
// identity slot gets a tap device and its own /30 out of Subnet.
type NetworkConfig struct {
	// Subnet is an IPv4 range reserved for sandbox links, e.g.
	// 172.31.0.0/16; it must not overlap any real network.
	Subnet netip.Prefix
	// ProxyPort is where the runner's egress proxy listens on each link's
	// host address.
	ProxyPort int
}

// NetInfo describes one sandbox link.
type NetInfo struct {
	Tap       string       `json:"tap"`
	Host      netip.Addr   `json:"host"`      // the proxy's address, on the host side
	Guest     netip.Prefix `json:"guest"`     // the guest's address and /30
	ProxyPort int          `json:"proxyPort"` //
}

// Validate checks c for identities sandbox slots.
func (c *NetworkConfig) Validate(identities int) error {
	if !c.Subnet.IsValid() || !c.Subnet.Addr().Is4() || c.Subnet.Bits() > 30 || c.Subnet.Masked() != c.Subnet {
		return fmt.Errorf("network subnet %s must be an IPv4 network of /30 or larger", c.Subnet)
	}
	if capacity := 1 << (32 - c.Subnet.Bits()) / 4; identities > capacity {
		return fmt.Errorf("network subnet %s has room for %d sandboxes, need %d", c.Subnet, capacity, identities)
	}
	if c.ProxyPort < 1024 || c.ProxyPort > 65535 {
		return errors.New("proxy port must be between 1024 and 65535 (the runner is unprivileged)")
	}
	return nil
}

// Link returns slot's link: tap hcc<slot>, host .1 and guest .2 of the
// slot's /30.
func (c *NetworkConfig) Link(slot int) NetInfo {
	base := c.Subnet.Addr().As4()
	n := binary.BigEndian.Uint32(base[:]) + uint32(slot)*4 //nolint:gosec // slot is validated against the subnet's capacity
	addr := func(off uint32) netip.Addr {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], n+off)
		return netip.AddrFrom4(b)
	}
	return NetInfo{
		Tap:       fmt.Sprintf("hcc%d", slot),
		Host:      addr(1),
		Guest:     netip.PrefixFrom(addr(2), 30),
		ProxyPort: c.ProxyPort,
	}
}

var tapRe = regexp.MustCompile(`^hcc[0-9]{1,4}$`)

// Valid checks a NetInfo received from the helper.
func (n NetInfo) Valid() error {
	if !tapRe.MatchString(n.Tap) || !n.Host.Is4() || !n.Guest.Addr().Is4() || n.Guest.Bits() != 30 ||
		n.Guest.Masked() != netip.PrefixFrom(n.Host, 30).Masked() || n.ProxyPort < 1024 || n.ProxyPort > 65535 {
		return fmt.Errorf("firecracker: invalid network info %+v", n)
	}
	return nil
}

// Ruleset is the nftables table guarding sandbox links: a guest may open
// new connections only to the proxy port on its own link's host address,
// nothing is forwarded to or from any sandbox link, and nothing arriving
// through any other interface may reach the sandbox subnet (so no other
// machine can use a sandbox's proxy). It is applied
// atomically, replacing any previous version.
func (c *NetworkConfig) Ruleset() string {
	return fmt.Sprintf(`table inet hcc_guard
delete table inet hcc_guard
table inet hcc_guard {
	chain input {
		type filter hook input priority filter; policy accept;
		iifname != "hcc*" ip daddr %s counter drop
		iifname "hcc*" ct state established,related accept
		iifname "hcc*" tcp dport %d fib daddr . iif type local ct state new accept
		iifname "hcc*" counter drop
	}
	chain forward {
		type filter hook forward priority filter; policy accept;
		iifname "hcc*" counter drop
		oifname "hcc*" counter drop
	}
}
`, c.Subnet, c.ProxyPort)
}
