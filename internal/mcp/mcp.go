// Package mcp reads Model Context Protocol messages (JSON-RPC 2.0) where
// the runner must understand them: tool calls that approval gates hold,
// and the requests and responses it relays for MCP servers run as
// plugins (ADR-0014).
package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Limits for showing tool calls.
const (
	MaxDetails  = 2048
	MaxToolName = 128
)

// ToolCall is one MCP tools/call in a JSON-RPC message.
type ToolCall struct {
	Tool string
	// Arguments as sent (compact JSON).
	Arguments string
}

// ToolCalls reads a JSON-RPC message (an object, or a batch) and returns
// its tool calls. It fails for anything it cannot read in full, so a
// gated call can never hide in a message it does not understand.
func ToolCalls(body []byte) ([]ToolCall, error) {
	body = bytes.TrimSpace(body)
	var msgs []json.RawMessage
	if len(body) > 0 && body[0] == '[' {
		if err := json.Unmarshal(body, &msgs); err != nil {
			return nil, fmt.Errorf("not a JSON-RPC batch: %w", err)
		}
	} else {
		msgs = []json.RawMessage{body}
	}
	var calls []ToolCall
	for _, m := range msgs {
		var msg struct {
			Method *string `json:"method"`
			Params struct {
				Name      *string         `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		if err := json.Unmarshal(m, &msg); err != nil {
			return nil, fmt.Errorf("not a JSON-RPC message: %w", err)
		}
		if msg.Method == nil || *msg.Method != "tools/call" {
			continue
		}
		if msg.Params.Name == nil || *msg.Params.Name == "" || len(*msg.Params.Name) > MaxToolName {
			return nil, errors.New("a tool call without a valid tool name")
		}
		args := "{}"
		if len(msg.Params.Arguments) > 0 {
			var buf bytes.Buffer
			if err := json.Compact(&buf, msg.Params.Arguments); err != nil {
				return nil, fmt.Errorf("tool arguments: %w", err)
			}
			args = buf.String()
		}
		calls = append(calls, ToolCall{Tool: *msg.Params.Name, Arguments: args})
	}
	return calls, nil
}

// DescribeToolCall is what a person approving a call sees; it must show
// the call in full.
func DescribeToolCall(c ToolCall) (string, error) {
	d := fmt.Sprintf("tool %s, arguments %s", c.Tool, c.Arguments)
	if len(d) > MaxDetails {
		return "", fmt.Errorf("its arguments are longer than %d bytes", MaxDetails)
	}
	return d, nil
}

// Message is a JSON-RPC message's envelope.
type Message struct {
	// ID is set for requests and responses (compact JSON).
	ID string
	// Method is set for requests and notifications.
	Method string
}

// Parse reads one JSON-RPC message (no batches: MCP's current transport
// sends one message per request).
func Parse(b []byte) (Message, error) {
	var m struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
	}
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '{' {
		return Message{}, errors.New("not a JSON-RPC message object")
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return Message{}, fmt.Errorf("not a JSON-RPC message: %w", err)
	}
	if m.JSONRPC != "2.0" {
		return Message{}, errors.New(`not JSON-RPC 2.0 ("jsonrpc": "2.0" missing)`)
	}
	out := Message{Method: m.Method}
	if len(m.ID) > 0 && string(m.ID) != "null" {
		var buf bytes.Buffer
		if err := json.Compact(&buf, m.ID); err != nil {
			return Message{}, err
		}
		out.ID = buf.String()
	}
	if out.ID == "" && out.Method == "" {
		return Message{}, errors.New("a JSON-RPC message with neither id nor method")
	}
	return out, nil
}

// IsRequest reports a method call that expects a response.
func (m Message) IsRequest() bool { return m.Method != "" && m.ID != "" }

// IsNotification reports a method call without a response.
func (m Message) IsNotification() bool { return m.Method != "" && m.ID == "" }

// IsResponse reports a result or error for a request.
func (m Message) IsResponse() bool { return m.Method == "" && m.ID != "" }

// ErrorResponse is a JSON-RPC error response to the request with id.
func ErrorResponse(id string, code int, message string) []byte {
	if id == "" {
		id = "null"
	}
	msg, _ := json.Marshal(message)
	return []byte(`{"jsonrpc":"2.0","id":` + id + `,"error":{"code":` + fmt.Sprint(code) + `,"message":` + string(msg) + `}}`)
}

// Error codes the runner answers with.
const (
	CodeNotApproved = -32001
	CodeUnavailable = -32002
)
