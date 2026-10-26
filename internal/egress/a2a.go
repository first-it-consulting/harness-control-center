package egress

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/first-it-consulting/harness-control-center/internal/mcp"
	"github.com/first-it-consulting/harness-control-center/internal/policy"
)

// maxA2ADetails bounds what a person approving an A2A call reads; a call
// that does not fit is refused, never shown in part.
const maxA2ADetails = 2048

// a2aCall is one JSON-RPC request to an A2A agent.
type a2aCall struct {
	Method string
	Params string // compact JSON
}

// a2aCalls reads a JSON-RPC message (or batch) to an A2A agent. It fails
// for anything it cannot read in full.
func a2aCalls(body []byte) ([]a2aCall, error) {
	body = bytes.TrimSpace(body)
	if err := mcp.StrictJSON(body); err != nil {
		return nil, err
	}
	var msgs []json.RawMessage
	if len(body) > 0 && body[0] == '[' {
		if err := json.Unmarshal(body, &msgs); err != nil {
			return nil, fmt.Errorf("not a JSON-RPC batch: %w", err)
		}
	} else {
		msgs = []json.RawMessage{body}
	}
	var calls []a2aCall
	for _, m := range msgs {
		var msg struct {
			Method *string         `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(m, &msg); err != nil {
			return nil, fmt.Errorf("not a JSON-RPC message: %w", err)
		}
		if msg.Method == nil || *msg.Method == "" {
			return nil, errors.New("a JSON-RPC message without a method")
		}
		params := "{}"
		if len(msg.Params) > 0 {
			var buf bytes.Buffer
			if err := json.Compact(&buf, msg.Params); err != nil {
				return nil, fmt.Errorf("params: %w", err)
			}
			params = buf.String()
		}
		calls = append(calls, a2aCall{Method: *msg.Method, Params: params})
	}
	return calls, nil
}

// inspectA2A reads a request to an A2A agent and adds the actions of its
// calls (a2a.<agent>.send|cancel|read), each described in full.
func (t *gatedTransport) inspectA2A(req *http.Request, ep Endpoint, actions, details []string) (*http.Request, []string, []string, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return req, actions, details, nil
	}
	body, err := readMCPBody(req.Body)
	if err != nil {
		return req, nil, nil, err
	}
	req = req.Clone(req.Context())
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	calls, err := a2aCalls(body)
	if err != nil {
		return req, nil, nil, err
	}
	for _, c := range calls {
		d := fmt.Sprintf("%s: %s %s", ep.Name, c.Method, c.Params)
		if len(d) > maxA2ADetails {
			return req, nil, nil, fmt.Errorf("its message is longer than %d bytes", maxA2ADetails)
		}
		actions = append(actions, policy.A2AAction(ep.Name, c.Method))
		details = append(details, d)
	}
	return req, actions, details, nil
}
