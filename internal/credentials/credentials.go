// Package credentials is the credential broker's store: named secrets,
// each bound to the hosts it may be sent to and the header it goes in.
// Secrets never enter a sandbox; the egress proxy adds them to requests
// for those hosts (see internal/egress).
package credentials

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

// Credential is one secret and where it may go.
type Credential struct {
	Name string
	// Hosts the secret may be sent to (exact names; no wildcards).
	Hosts []string
	// Scheme and Port say how the hosts are reached: "https" (default,
	// port 443) or, only if the operator says so explicitly, "http" (the
	// secret then crosses the network unencrypted).
	Scheme string
	Port   int
	// Header carries the secret, e.g. "x-api-key" or "Authorization".
	Header string
	// Value is the full header value, e.g. "Bearer <secret>".
	Value string
}

// String never shows the secret.
func (c Credential) String() string {
	return fmt.Sprintf("credential %s (%s for %s://%s:%d)", c.Name, c.Header, c.Scheme, strings.Join(c.Hosts, ", "), c.Port)
}

// GoString never shows the secret either (%#v).
func (c Credential) GoString() string { return c.String() }

type fileEntry struct {
	Hosts      []string `yaml:"hosts"`
	Scheme     string   `yaml:"scheme"`
	Port       int      `yaml:"port"`
	Header     string   `yaml:"header"`
	Prefix     string   `yaml:"prefix"`
	SecretFile string   `yaml:"secretFile"`
}

var (
	nameRe   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	headerRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	// Headers whose meaning the proxy controls.
	reservedHeaders = map[string]bool{
		"host": true, "content-length": true, "transfer-encoding": true, "connection": true,
		"upgrade": true, "te": true, "trailer": true, "proxy-authorization": true, "keep-alive": true,
	}
)

// MaxSecretBytes bounds a secret file.
const MaxSecretBytes = 16 << 10

// Load reads the store file and every secret it names. Unknown fields are
// errors, as everywhere else. Secret files must not be readable by
// others (checkPrivate).
func Load(path string) (map[string]Credential, error) {
	data, err := readLimited(path, 1<<20)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var entries map[string]fileEntry
	if err := dec.Decode(&entries); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("credentials: %s: %w", path, err)
	}
	out := make(map[string]Credential, len(entries))
	var errs []error
	for name, e := range entries {
		c, err := build(name, e)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out[name] = c
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return out, nil
}

func build(name string, e fileEntry) (Credential, error) {
	fail := func(format string, args ...any) (Credential, error) {
		return Credential{}, fmt.Errorf("credentials: %s: %s", name, fmt.Sprintf(format, args...))
	}
	if !nameRe.MatchString(name) {
		return fail("invalid name")
	}
	if len(e.Hosts) == 0 {
		return fail("at least one host is required")
	}
	hosts := make([]string, 0, len(e.Hosts))
	for _, h := range e.Hosts {
		n := spec.NormalizeHost(h)
		if err := spec.ValidateHostName(n); err != nil {
			return fail("host %q: %v (exact names only)", h, err)
		}
		hosts = append(hosts, n)
	}
	scheme, port := e.Scheme, e.Port
	switch scheme {
	case "", "https":
		scheme = "https"
		if port == 0 {
			port = 443
		}
	case "http":
		if port == 0 {
			port = 80
		}
	default:
		return fail("scheme must be https or http")
	}
	if port < 1 || port > 65535 {
		return fail("invalid port %d", port)
	}
	if !headerRe.MatchString(e.Header) || reservedHeaders[strings.ToLower(e.Header)] {
		return fail("header %q is not allowed", e.Header)
	}
	if strings.ContainsAny(e.Prefix, "\r\n\x00") {
		return fail("prefix contains control characters")
	}
	if !filepath.IsAbs(e.SecretFile) || filepath.Clean(e.SecretFile) != e.SecretFile {
		return fail("secretFile must be a clean absolute path")
	}
	if err := checkPrivate(e.SecretFile); err != nil {
		return fail("%v", err)
	}
	raw, err := readLimited(e.SecretFile, MaxSecretBytes)
	if err != nil {
		return fail("%v", err)
	}
	secret := strings.TrimSpace(string(raw))
	if secret == "" {
		return fail("secret is empty")
	}
	// A newline in a header value would let the secret file inject
	// headers of its own.
	if strings.ContainsAny(secret, "\r\n\x00") {
		return fail("secret contains control characters")
	}
	return Credential{Name: name, Hosts: hosts, Scheme: scheme, Port: port, Header: e.Header, Value: e.Prefix + secret}, nil
}

func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // operator-configured paths
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, limit)
	}
	return data, nil
}

// ForHarness returns the credentials h is granted, checking that each
// exists and that every host (and port) it would be sent to is in h's
// egress allowlist (otherwise the grant is meaningless or wider than
// reviewed). The result maps host name to credential.
func ForHarness(store map[string]Credential, h *spec.Harness, allowed func(host string, port int) bool) (map[string]Credential, error) {
	byHost := map[string]Credential{}
	var errs []error
	for _, name := range h.Spec.Permissions.Credentials {
		c, ok := store[name]
		if !ok {
			errs = append(errs, fmt.Errorf("credential %q does not exist on this runner", name))
			continue
		}
		for _, host := range c.Hosts {
			if !allowed(host, c.Port) {
				errs = append(errs, fmt.Errorf("credential %q is for %s, which the harness may not reach on port %d", name, host, c.Port))
				continue
			}
			if prev, dup := byHost[host]; dup {
				errs = append(errs, fmt.Errorf("credentials %q and %q are both for %s", prev.Name, name, host))
				continue
			}
			byHost[host] = c
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("credentials: %w", err)
	}
	return byHost, nil
}
