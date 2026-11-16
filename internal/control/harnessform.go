package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/first-it-consulting/harness-control-center/internal/policy"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

// The harness form edits a harness as plain data (the YAML document as
// JSON); these two calls convert between that and YAML. The server stays
// the judge: the YAML is checked by spec.Parse and the policy engine,
// exactly as one typed by hand.

// maxFormDepth bounds the nesting of a form document (a harness is at
// most six levels deep).
const maxFormDepth = 12

// keyOrder puts a harness's keys in the order people read them; others
// follow alphabetically.
var keyOrder = []string{
	"apiVersion", "kind", "metadata", "spec",
	"name", "description",
	"runtime", "target", "prompt", "model", "trigger", "sandbox", "budget", "permissions", "approvals", "mcp", "a2a",
	"endpoint", "credential", "contextWindow", "smallModel", "maxOutputTokens", "thinkingTokens",
	"schedule", "loop", "maxIterations", "interval",
	"image", "cpus", "memoryMiB", "diskMiB", "timeout",
	"maxCostUSD", "maxTokens",
	"network", "allowPrivateNetworks", "egress", "host", "ports", "credentials", "tools", "install", "subagents", "max",
	"requireFor", "servers", "agents", "url", "plugin",
}

func keyRank(k string) int {
	if i := slices.Index(keyOrder, k); i >= 0 {
		return i
	}
	return len(keyOrder)
}

// formDoc is a decoded harness for the form.
type formDoc struct {
	Doc any `json:"doc"`
}

// harnessDecode turns YAML into the form's data. YAML that is not a
// harness-shaped mapping is refused (the YAML tab says why).
func (s *Server) harnessDecode(w http.ResponseWriter, r *http.Request) {
	var b yamlBody
	if !readJSON(w, r, &b) {
		return
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(b.YAML), &doc); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("the YAML cannot be read: %w", err))
		return
	}
	if doc == nil {
		doc = map[string]any{}
	}
	writeJSON(w, http.StatusOK, formDoc{Doc: doc})
}

type encodeRequest struct {
	Doc json.RawMessage `json:"doc"`
}

type encodeResult struct {
	YAML  string `json:"yaml"`
	Check Check  `json:"check"`
}

// harnessEncode turns the form's data into YAML and checks it.
func (s *Server) harnessEncode(w http.ResponseWriter, r *http.Request) {
	var req encodeRequest
	if !readJSON(w, r, &req) {
		return
	}
	out, err := encodeForm(req.Doc)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	c, _ := checkHarness(out)
	writeJSON(w, http.StatusOK, encodeResult{YAML: out, Check: c})
}

// encodeForm returns the YAML of a form document: keys in reading order,
// empty values left out (a harness's defaults are its zero values), the
// prompt as a block.
func encodeForm(raw json.RawMessage) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", fmt.Errorf("invalid form data: %w", err)
	}
	if _, ok := v.(map[string]any); !ok {
		return "", errors.New("invalid form data: not an object")
	}
	n, err := formNode(v, 0)
	if err != nil {
		return "", err
	}
	if n == nil {
		n = &yaml.Node{Kind: yaml.MappingNode}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// formNode converts a JSON value; nil for an empty one.
func formNode(v any, depth int) (*yaml.Node, error) {
	if depth > maxFormDepth {
		return nil, errors.New("invalid form data: nested too deeply")
	}
	switch x := v.(type) {
	case nil:
		return nil, nil
	case bool:
		if !x {
			return nil, nil
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"}, nil
	case json.Number:
		if x == "0" {
			return nil, nil
		}
		tag := "!!int"
		if strings.ContainsAny(string(x), ".eE") {
			tag = "!!float"
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: string(x)}, nil
	case string:
		if strings.TrimSpace(x) == "" {
			return nil, nil
		}
		n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: x}
		if strings.Contains(x, "\n") {
			n.Style = yaml.LiteralStyle
		}
		return n, nil
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, e := range x {
			c, err := formNode(e, depth+1)
			if err != nil {
				return nil, err
			}
			if c != nil {
				n.Content = append(n.Content, c)
			}
		}
		if len(n.Content) == 0 {
			return nil, nil
		}
		// Short lists of plain values read best on one line.
		if len(n.Content) <= 8 && !slices.ContainsFunc(n.Content, func(c *yaml.Node) bool { return c.Kind != yaml.ScalarNode }) {
			n.Style = yaml.FlowStyle
		}
		return n, nil
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, func(a, b string) int {
			if ra, rb := keyRank(a), keyRank(b); ra != rb {
				return ra - rb
			}
			return strings.Compare(a, b)
		})
		n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		for _, k := range keys {
			c, err := formNode(x[k], depth+1)
			if err != nil {
				return nil, err
			}
			if c != nil {
				n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, c)
			}
		}
		if len(n.Content) == 0 {
			return nil, nil
		}
		return n, nil
	}
	return nil, fmt.Errorf("invalid form data: unexpected %T", v)
}

// The form's choices that come from the Control Center itself: harness
// limits, so the form shows the same bounds the parser enforces.
type formLimits struct {
	MaxCPUs        int      `json:"maxCPUs"`
	MaxMemoryMiB   int      `json:"maxMemoryMiB"`
	MaxDiskMiB     int      `json:"maxDiskMiB"`
	MaxTimeout     string   `json:"maxTimeout"`
	MaxCostUSD     float64  `json:"maxCostUSD"`
	MaxTokens      int64    `json:"maxTokens"`
	MaxSubagents   int      `json:"maxSubagents"`
	MaxIterations  int      `json:"maxIterations"`
	ApprovalAction []string `json:"approvalActions"`
	Defaults       struct {
		Image      string  `json:"image"`
		CPUs       int     `json:"cpus"`
		MemoryMiB  int     `json:"memoryMiB"`
		DiskMiB    int     `json:"diskMiB"`
		Timeout    string  `json:"timeout"`
		MaxCostUSD float64 `json:"maxCostUSD"`
		MaxTokens  int64   `json:"maxTokens"`
	} `json:"defaults"`
}

func (s *Server) harnessLimits(w http.ResponseWriter, _ *http.Request) {
	l := formLimits{
		MaxCPUs: spec.MaxCPUs, MaxMemoryMiB: spec.MaxMemoryMiB, MaxDiskMiB: spec.MaxDiskMiB,
		MaxTimeout: spec.MaxTimeout.String(), MaxCostUSD: spec.MaxCostUSD, MaxTokens: spec.MaxTokens,
		MaxSubagents: spec.MaxSubagents, MaxIterations: spec.MaxLoopIteration,
		ApprovalAction: policy.ApprovalActions,
	}
	l.Defaults.Image, l.Defaults.CPUs, l.Defaults.MemoryMiB, l.Defaults.DiskMiB = spec.DefaultImage, spec.DefaultCPUs, spec.DefaultMemoryMiB, spec.DefaultDiskMiB
	l.Defaults.Timeout, l.Defaults.MaxCostUSD, l.Defaults.MaxTokens = spec.DefaultTimeout.String(), spec.DefaultMaxCostUSD, spec.DefaultMaxTokens
	writeJSON(w, http.StatusOK, l)
}
