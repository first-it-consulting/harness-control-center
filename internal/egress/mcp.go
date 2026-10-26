package egress

import (
	"fmt"
	"io"
	"strings"
)

// maxMCPBody bounds an MCP request read for inspection.
const maxMCPBody = 1 << 20

// MCPEndpoint is a remote MCP server of the harness (spec.mcp): POSTs to
// it are JSON-RPC messages whose tool calls approval gates can hold
// (ADR-0014).
type MCPEndpoint struct {
	Name string
	Host string
	Port int
	Path string
}

// mcpEndpoint returns the MCP server a request goes to, if any.
func (p *Proxy) mcpEndpoint(host string, port int, path string) (MCPEndpoint, bool) {
	for _, ep := range p.MCP {
		if ep.Host == host && ep.Port == port && strings.TrimSuffix(ep.Path, "/") == strings.TrimSuffix(path, "/") {
			return ep, true
		}
	}
	return MCPEndpoint{}, false
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
