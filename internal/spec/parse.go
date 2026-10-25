package spec

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"time"

	"go.yaml.in/yaml/v3"
)

// MaxDocumentBytes bounds the input size to keep parsing cheap and
// predictable, whatever the source of the document.
const MaxDocumentBytes = 1 << 20

// ErrTooLarge is returned for documents over MaxDocumentBytes.
var ErrTooLarge = errors.New("spec: document exceeds maximum size")

// Parse decodes, defaults and validates a single harness document.
// The returned harness is safe to hand to the policy engine.
func Parse(data []byte) (*Harness, error) {
	if len(data) > MaxDocumentBytes {
		return nil, ErrTooLarge
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var h Harness
	if err := dec.Decode(&h); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("spec: empty document")
		}
		return nil, fmt.Errorf("spec: %w", err)
	}
	// Exactly one document: a second one could otherwise smuggle in
	// settings a reviewer of the first document never sees.
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("spec: multiple documents are not allowed")
	}

	h.ApplyDefaults()
	if err := h.Validate(); err != nil {
		return nil, err
	}
	return &h, nil
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like \"30m\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = v
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (d Duration) MarshalYAML() (any, error) {
	return d.String(), nil
}
