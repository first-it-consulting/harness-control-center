//go:build unix

package credentials

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/first-it-consulting/harness-control-center/internal/policy"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

const testSecret = "sk-test-0123456789"

type store struct {
	dir string
}

func newStore(t *testing.T) *store {
	t.Helper()
	return &store{dir: t.TempDir()}
}

func (s *store) secret(t *testing.T, name, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(s.dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func (s *store) file(t *testing.T, yaml string) string {
	t.Helper()
	p := filepath.Join(s.dir, "credentials.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	s := newStore(t)
	sec := s.secret(t, "anthropic", testSecret+"\n", 0o640)
	gl := s.secret(t, "gitlab", "glpat-xyz", 0o600)
	creds, err := Load(s.file(t, fmt.Sprintf(`
anthropic:
  hosts: [API.Anthropic.com.]
  header: x-api-key
  secretFile: %s
gitlab-main:
  hosts: [gitlab.com]
  header: Authorization
  prefix: "Bearer "
  secretFile: %s
`, sec, gl)))
	if err != nil {
		t.Fatal(err)
	}
	a := creds["anthropic"]
	if a.Value != testSecret || a.Hosts[0] != "api.anthropic.com" || a.Header != "x-api-key" {
		t.Errorf("anthropic = %#v / %q", a, a.Hosts)
	}
	if g := creds["gitlab-main"]; g.Value != "Bearer glpat-xyz" {
		t.Errorf("gitlab value not prefixed")
	}
	// Formatting must never reveal the secret.
	for _, f := range []string{"%v", "%+v", "%#v", "%s"} {
		if strings.Contains(fmt.Sprintf(f, a), testSecret) {
			t.Errorf("%s reveals the secret", f)
		}
	}
}

func TestLoadRejects(t *testing.T) {
	s := newStore(t)
	good := s.secret(t, "good", testSecret, 0o600)
	tests := map[string]struct {
		yaml string
		want string
	}{
		"world-readable secret": {fmt.Sprintf("a: {hosts: [x.example], header: x-k, secretFile: %s}", s.secret(t, "w", testSecret, 0o644)), "accessible by others"},
		"group-writable secret": {fmt.Sprintf("a: {hosts: [x.example], header: x-k, secretFile: %s}", s.secret(t, "g", testSecret, 0o660)), "writable by its group"},
		"newline injection":     {fmt.Sprintf("a: {hosts: [x.example], header: x-k, secretFile: %s}", s.secret(t, "n", "abc\r\nX-Evil: 1", 0o600)), "control characters"},
		"empty secret":          {fmt.Sprintf("a: {hosts: [x.example], header: x-k, secretFile: %s}", s.secret(t, "e", "  \n", 0o600)), "empty"},
		"wildcard host":         {fmt.Sprintf("a: {hosts: ['*.example.com'], header: x-k, secretFile: %s}", good), "exact names"},
		"ip host":               {fmt.Sprintf("a: {hosts: [1.2.3.4], header: x-k, secretFile: %s}", good), "host"},
		"no hosts":              {fmt.Sprintf("a: {header: x-k, secretFile: %s}", good), "at least one host"},
		"reserved header":       {fmt.Sprintf("a: {hosts: [x.example], header: Host, secretFile: %s}", good), "not allowed"},
		"header injection":      {fmt.Sprintf("a: {hosts: [x.example], header: 'x-k: y', secretFile: %s}", good), "not allowed"},
		"prefix injection":      {fmt.Sprintf("a: {hosts: [x.example], header: x-k, prefix: \"a\\r\\nb\", secretFile: %s}", good), "control characters"},
		"relative secret file":  {"a: {hosts: [x.example], header: x-k, secretFile: secret}", "absolute"},
		"unknown field":         {fmt.Sprintf("a: {hosts: [x.example], header: x-k, secretFile: %s, value: oops}", good), "not found"},
		"bad name":              {fmt.Sprintf("A_B: {hosts: [x.example], header: x-k, secretFile: %s}", good), "invalid name"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Load(s.file(t, tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Load = %v, want error containing %q", err, tt.want)
			}
			if err != nil && strings.Contains(err.Error(), testSecret) {
				t.Error("error message reveals the secret")
			}
		})
	}
	link := filepath.Join(s.dir, "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(s.file(t, fmt.Sprintf("a: {hosts: [x.example], header: x-k, secretFile: %s}", link))); err == nil {
		t.Error("symlinked secret file accepted")
	}
}

func TestForHarness(t *testing.T) {
	store := map[string]Credential{
		"anthropic": {Name: "anthropic", Hosts: []string{"api.anthropic.com"}, Header: "x-api-key", Value: "s1"},
		"gitlab":    {Name: "gitlab", Hosts: []string{"gitlab.com"}, Header: "Authorization", Value: "s2"},
		"dup":       {Name: "dup", Hosts: []string{"api.anthropic.com"}, Header: "x-other", Value: "s3"},
	}
	harness := func(creds, egress string) *spec.Harness {
		t.Helper()
		h, err := spec.Parse([]byte(fmt.Sprintf(`apiVersion: hcc/v1alpha1
kind: Harness
metadata: {name: t}
spec:
  runtime: claude-code
  prompt: p
  permissions:
    credentials: [%s]
    network: {egress: [%s]}
`, creds, egress)))
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	check := func(h *spec.Harness) func(string, int) bool {
		e, _ := policy.New(h)
		return func(host string, port int) bool { return e.CheckEgress(host, port).Allowed }
	}

	h := harness("anthropic", "{host: api.anthropic.com}, {host: gitlab.com}")
	byHost, err := ForHarness(store, h, check(h))
	if err != nil {
		t.Fatal(err)
	}
	if len(byHost) != 1 || byHost["api.anthropic.com"].Name != "anthropic" {
		t.Errorf("byHost = %v (only granted credentials may be used)", byHost)
	}

	for name, tt := range map[string]struct{ creds, egress, want string }{
		"missing":          {"nope", "{host: gitlab.com}", "does not exist"},
		"host not allowed": {"gitlab", "{host: api.anthropic.com}", "may not reach"},
		"same host twice":  {"anthropic, dup", "{host: api.anthropic.com}", "both for"},
	} {
		h := harness(tt.creds, tt.egress)
		if _, err := ForHarness(store, h, check(h)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want %q", name, err, tt.want)
		}
	}
}
