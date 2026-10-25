package egress

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"slices"
	"sync"
	"time"
)

// CA is a job's ephemeral certificate authority for the hosts whose
// requests get credentials. Its key exists only in the runner's memory,
// for one job. X.509 name constraints limit it to exactly those hosts
// (and forbid IP addresses), so a guest trusting it trusts nothing else.
type CA struct {
	cert   *x509.Certificate
	key    *ecdsa.PrivateKey
	pem    []byte
	hosts  []string
	expiry time.Time

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

// caValidity covers the longest possible job (24h) with margin.
const caValidity = 48 * time.Hour

// NewCA creates a CA limited to hosts.
func NewCA(hosts []string, now time.Time) (*CA, error) {
	if len(hosts) == 0 {
		return nil, errors.New("egress: a CA needs at least one host")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:                serial(),
		Subject:                     pkix.Name{CommonName: "hcc job CA (credential broker)"},
		NotBefore:                   now.Add(-time.Hour),
		NotAfter:                    now.Add(caValidity),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLenZero:              true,
		KeyUsage:                    x509.KeyUsageCertSign,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         slices.Clone(hosts),
		ExcludedIPRanges: []*net.IPNet{
			{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
			{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)},
		},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{
		cert: cert, key: key, hosts: slices.Clone(hosts), expiry: tmpl.NotAfter,
		pem:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		leaves: map[string]*tls.Certificate{},
	}, nil
}

// PEM is the CA certificate for the guest to trust (public, no secret).
func (c *CA) PEM() []byte { return slices.Clone(c.pem) }

// Leaf returns a server certificate for host, one of the CA's hosts.
func (c *CA) Leaf(host string) (*tls.Certificate, error) {
	if !slices.Contains(c.hosts, host) {
		return nil, fmt.Errorf("egress: no certificate for %s", host)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if leaf, ok := c.leaves[host]; ok {
		return leaf, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    c.cert.NotBefore,
		NotAfter:     c.expiry,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	leaf := &tls.Certificate{Certificate: [][]byte{der, c.cert.Raw}, PrivateKey: key}
	c.leaves[host] = leaf
	return leaf, nil
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return n
}
