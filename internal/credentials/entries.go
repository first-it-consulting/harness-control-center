package credentials

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Entry is a credential as the store lists it: everything but the
// secret, which is write-only (ADR-0017). It is what the Control Center
// shows and edits.
type Entry struct {
	Name string `json:"name"`
	// Kind is "" (added to web requests) or KindSecret (for the agent).
	Kind   string   `json:"kind,omitempty"`
	Hosts  []string `json:"hosts"`
	Scheme string   `json:"scheme"`
	Port   int      `json:"port"`
	Header string   `json:"header"`
	Prefix string   `json:"prefix,omitempty"`
	// Updated is when its secret was last set.
	Updated time.Time `json:"updated,omitzero"`
}

// List returns the store's credentials without their secrets, sorted by
// name. The secrets are checked as the runner checks them.
func List(path string) ([]Entry, error) {
	creds, err := Load(path)
	if err != nil {
		return nil, err
	}
	entries, err := readEntries(path)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(creds))
	for name, c := range creds {
		hosts := c.Hosts
		if hosts == nil {
			hosts = []string{}
		}
		e := Entry{Name: name, Kind: c.Kind, Hosts: hosts, Scheme: c.Scheme, Port: c.Port, Header: c.Header, Prefix: entries[name].Prefix}
		if fi, err := os.Stat(entries[name].SecretFile); err == nil {
			e.Updated = fi.ModTime().UTC()
		}
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func readEntries(path string) (map[string]fileEntry, error) {
	data, err := readLimited(path, 1<<20)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	entries := map[string]fileEntry{}
	if err := dec.Decode(&entries); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("credentials: %s: %w", path, err)
	}
	if entries == nil {
		entries = map[string]fileEntry{}
	}
	return entries, nil
}
