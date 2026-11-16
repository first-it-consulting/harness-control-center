package control

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

// The model catalog: the endpoints and models a person picks from when
// they make a harness. A harness gets a copy of the model it uses (its
// spec.model), so it runs the same on every runner; the catalog only
// saves typing. It holds no secrets: an endpoint names the credential a
// runner adds for it, and the secret stays on the runner.

// Endpoint kinds, which decide what an endpoint may be.
const (
	// EndpointAnthropic is Anthropic's own API.
	EndpointAnthropic = "anthropic"
	// EndpointNetwork is a model server in the person's own network
	// (oMLX, LM Studio, LiteLLM …): it may be internal and unencrypted.
	EndpointNetwork = "network"
	// EndpointOther is another service that speaks Anthropic's API.
	EndpointOther = "other"
)

// Bounds of the catalog.
const (
	MaxEndpoints     = 32
	MaxCatalogModels = 128
	maxLabel         = 80
)

// CatalogEndpoint is where models are reached.
type CatalogEndpoint struct {
	Name string `yaml:"name" json:"name"`
	Kind string `yaml:"kind" json:"kind"`
	URL  string `yaml:"url" json:"url"`
	// Credential names the runners' credential for this endpoint.
	Credential string `yaml:"credential,omitempty" json:"credential,omitempty"`
}

// CatalogModel is a model at an endpoint, with what the runtime should
// know about it (see spec.Model).
type CatalogModel struct {
	// ID is the model's name at the endpoint.
	ID string `yaml:"id" json:"id"`
	// Label is what people see; empty means the ID.
	Label           string `yaml:"label,omitempty" json:"label,omitempty"`
	Endpoint        string `yaml:"endpoint" json:"endpoint"`
	ContextWindow   int    `yaml:"contextWindow,omitempty" json:"contextWindow,omitempty"`
	SmallModel      string `yaml:"smallModel,omitempty" json:"smallModel,omitempty"`
	MaxOutputTokens int    `yaml:"maxOutputTokens,omitempty" json:"maxOutputTokens,omitempty"`
	ThinkingTokens  int    `yaml:"thinkingTokens,omitempty" json:"thinkingTokens,omitempty"`
}

// Catalog is models.yaml.
type Catalog struct {
	Endpoints []CatalogEndpoint `yaml:"endpoints" json:"endpoints"`
	Models    []CatalogModel    `yaml:"models" json:"models"`
}

// Endpoint returns the endpoint called name.
func (c *Catalog) Endpoint(name string) (CatalogEndpoint, bool) {
	for _, e := range c.Endpoints {
		if e.Name == name {
			return e, true
		}
	}
	return CatalogEndpoint{}, false
}

// Spec returns the spec.model a harness gets for m, and whether its
// endpoint is in the person's own network (the harness then needs
// network.allowPrivateNetworks).
func (c *Catalog) Spec(m CatalogModel) (spec.Model, bool) {
	e, _ := c.Endpoint(m.Endpoint)
	return spec.Model{
		Name: m.ID, Endpoint: e.URL, Credential: e.Credential, ContextWindow: m.ContextWindow,
		SmallModel: m.SmallModel, MaxOutputTokens: m.MaxOutputTokens, ThinkingTokens: m.ThinkingTokens,
	}, e.Kind == EndpointNetwork
}

// Validate checks the catalog: every model as a harness would check it.
func (c *Catalog) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if len(c.Endpoints) > MaxEndpoints {
		add("at most %d endpoints", MaxEndpoints)
	}
	if len(c.Models) > MaxCatalogModels {
		add("at most %d models", MaxCatalogModels)
	}
	seen := map[string]bool{}
	for _, e := range c.Endpoints {
		what := "endpoint " + e.Name
		switch {
		case !nameRe.MatchString(e.Name):
			add("endpoint name %q must be 1-63 lowercase letters, digits or '-'", e.Name)
		case seen[e.Name]:
			add("%s: there is another endpoint with that name", what)
		}
		seen[e.Name] = true
		if e.Kind != EndpointAnthropic && e.Kind != EndpointNetwork && e.Kind != EndpointOther {
			add("%s: unknown kind %q", what, e.Kind)
		}
		// Unencrypted only within the person's own network.
		if u, err := url.Parse(e.URL); err == nil && u.Scheme == "http" && e.Kind != EndpointNetwork {
			add("%s: must be https:// (only a model server on your own network may use http://)", what)
		}
		m := spec.Model{Name: "model", Endpoint: e.URL, Credential: e.Credential}
		if err := spec.ValidateModel(&m, e.Kind == EndpointNetwork); err != nil {
			add("%s: %s", what, fieldErrors(err))
		}
	}
	models := map[string]bool{}
	for _, m := range c.Models {
		what := "model " + m.ID
		if len(m.Label) > maxLabel || strings.ContainsAny(m.Label, "\r\n") {
			add("%s: the name shown must be one line of at most %d characters", what, maxLabel)
		}
		if _, ok := c.Endpoint(m.Endpoint); !ok {
			add("%s: no endpoint called %q", what, m.Endpoint)
			continue
		}
		key := m.Endpoint + "\x00" + m.ID
		if models[key] {
			add("%s: listed twice for endpoint %s", what, m.Endpoint)
		}
		models[key] = true
		sm, private := c.Spec(m)
		if err := spec.ValidateModel(&sm, private); err != nil {
			add("%s: %s", what, fieldErrors(err))
		}
	}
	return errors.Join(errs...)
}

// fieldErrors is err's messages without the "spec.model." prefix.
func fieldErrors(err error) string {
	var joined interface{ Unwrap() []error }
	if !errors.As(err, &joined) {
		return strings.TrimPrefix(err.Error(), "spec.model.")
	}
	parts := make([]string, 0, len(joined.Unwrap()))
	for _, e := range joined.Unwrap() {
		parts = append(parts, strings.TrimPrefix(e.Error(), "spec.model."))
	}
	return strings.Join(parts, "; ")
}

// modelsPath is the catalog next to control.yaml.
func (s *Server) modelsPath() string { return filepath.Join(filepath.Dir(s.cfgPath), "models.yaml") }

// LoadCatalog reads a catalog file; a missing one is empty.
func LoadCatalog(path string) (*Catalog, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the user's own file
	if errors.Is(err, os.ErrNotExist) {
		return &Catalog{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Catalog
	if len(bytes.TrimSpace(data)) > 0 {
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// CatalogView is what GET /api/models answers: the catalog, and which
// saved harnesses use each model (by index in Models).
type CatalogView struct {
	Catalog
	UsedBy [][]string `json:"usedBy"`
}

func (s *Server) modelsGet(w http.ResponseWriter, _ *http.Request) {
	s.modelsMu.Lock()
	c, err := LoadCatalog(s.modelsPath())
	s.modelsMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, s.catalogView(c))
}

func (s *Server) modelsPut(w http.ResponseWriter, r *http.Request) {
	var c Catalog
	if !readJSON(w, r, &c) {
		return
	}
	if err := c.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the models are not valid", "errors": splitLines(err)})
		return
	}
	data, err := yaml.Marshal(&c)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.modelsMu.Lock()
	err = writeFileAtomic(s.modelsPath(), data)
	s.modelsMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Info("model catalog saved", "endpoints", len(c.Endpoints), "models", len(c.Models))
	writeJSON(w, http.StatusOK, s.catalogView(&c))
}

// catalogView adds which saved harnesses use each model: those whose
// spec.model has its ID and endpoint URL.
func (s *Server) catalogView(c *Catalog) CatalogView {
	v := CatalogView{Catalog: *c, UsedBy: make([][]string, len(c.Models))}
	if v.Endpoints == nil {
		v.Endpoints = []CatalogEndpoint{}
	}
	if v.Models == nil {
		v.Models = []CatalogModel{}
	}
	entries, err := s.lib.List()
	if err != nil {
		return v
	}
	for _, e := range entries {
		if e.Error != "" {
			continue
		}
		data, err := s.lib.Get(e.Name)
		if err != nil {
			continue
		}
		h, err := spec.Parse(data)
		if err != nil || h.Spec.Model == nil {
			continue
		}
		for i, m := range c.Models {
			if ep, _ := c.Endpoint(m.Endpoint); h.Spec.Model.Name == m.ID && h.Spec.Model.Endpoint == ep.URL {
				v.UsedBy[i] = append(v.UsedBy[i], e.Name)
			}
		}
	}
	for i := range v.UsedBy {
		if v.UsedBy[i] == nil {
			v.UsedBy[i] = []string{}
		}
	}
	return v
}

// splitLines is a joined error's messages.
func splitLines(err error) []string {
	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		var out []string
		for _, e := range joined.Unwrap() {
			out = append(out, splitLines(e)...)
		}
		return out
	}
	return []string{err.Error()}
}
