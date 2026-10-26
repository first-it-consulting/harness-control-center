package egress

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
)

// maxMCPBody bounds an MCP request read for inspection.
const maxMCPBody = 1 << 20

// Endpoint is a remote MCP server (spec.mcp, ADR-0014) or A2A agent
// (spec.a2a, ADR-0015) of the harness: POSTs to it are JSON-RPC messages
// whose calls approval gates can hold.
type Endpoint struct {
	Name string
	Host string
	Port int
	Path string
}

const (
	kindMCP = "mcp"
	kindA2A = "a2a"
)

type gatedEndpoint struct {
	Endpoint
	kind string
}

// gatedEndpoints are the MCP servers and A2A agents on host:port whose
// calls the job's approval gates can hold.
func (p *Proxy) gatedEndpoints(host string, port int) []gatedEndpoint {
	var out []gatedEndpoint
	add := func(list []Endpoint, kind string) {
		for _, ep := range list {
			if ep.Host == host && ep.Port == port {
				out = append(out, gatedEndpoint{Endpoint: ep, kind: kind})
			}
		}
	}
	if p.Policy.GatesMCP() {
		add(p.MCP, kindMCP)
	}
	if p.Policy.GatesA2A() {
		add(p.A2A, kindA2A)
	}
	return out
}

// pickEndpoint decides which endpoint a request is for. Paths are
// compared as a lenient server might (cleaned, slashes collapsed, case
// folded); a host with one endpoint is assumed to answer on any path; with
// several, a path naming none of them is refused rather than guessed.
func pickEndpoint(eps []gatedEndpoint, p string) (gatedEndpoint, error) {
	want := canonicalPath(p)
	for _, ep := range eps {
		if canonicalPath(ep.Path) == want {
			return ep, nil
		}
	}
	if len(eps) == 1 {
		return eps[0], nil
	}
	return gatedEndpoint{}, errors.New("this host serves several MCP servers or A2A agents, and the path names none of them")
}

func canonicalPath(p string) string {
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	return strings.ToLower(strings.TrimSuffix(path.Clean("/"+p), "/"))
}

func hasBody(req *http.Request) bool {
	return req.ContentLength != 0 && req.Body != nil && req.Body != http.NoBody
}

// readMCPBody reads a request body for inspection (at most maxMCPBody).
func readMCPBody(body io.ReadCloser) ([]byte, error) {
	defer body.Close()
	b, err := io.ReadAll(io.LimitReader(body, maxMCPBody+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxMCPBody {
		return nil, fmt.Errorf("it is larger than %d bytes", maxMCPBody)
	}
	return b, nil
}
