package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/first-it-consulting/harness-control-center/internal/safetext"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

// Model providers: the services harnesses reach models at (Anthropic's
// API, Ollama, oMLX, LiteLLM …). A harness picks a provider and one of its
// models, and gets a copy (its spec.model), so it runs the same on every
// runner. The Control Center asks a provider for its models with the
// provider's credential from the store, and only if that credential may
// go to the provider's host. Claude Code speaks Anthropic's Messages API,
// so a provider must offer it.

// Provider types: what kind of server it is (for its model list and the
// defaults the UI offers).
var providerTypes = []string{"anthropic", "ollama", "omlx", "litellm", "lmstudio", "openrouter", "other"}

// Bounds.
const (
	MaxProviders      = 32
	maxProviderModels = 500
	maxModelsBody     = 4 << 20
	providerTimeout   = 15 * time.Second
)

// Provider is one model service.
type Provider struct {
	Name string `yaml:"name" json:"name"`
	Type string `yaml:"type" json:"type"`
	URL  string `yaml:"url" json:"url"`
	// Credential names a credential of the Control Center's store.
	Credential string `yaml:"credential,omitempty" json:"credential,omitempty"`
	// Local: it is in the person's own network (a harness using it then
	// needs network.allowPrivateNetworks). Internal names (*.lan …) are
	// local anyway; this covers a home server with a public-looking name.
	Local bool `yaml:"local,omitempty" json:"local,omitempty"`
	// Disabled: kept, but not offered to harnesses.
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`
}

// Private reports whether the provider is in the person's own network.
func (p Provider) Private() bool {
	if p.Local {
		return true
	}
	u, err := url.Parse(p.URL)
	return err == nil && spec.IsPrivateName(spec.NormalizeHost(u.Hostname()))
}

// Catalog is models.yaml: the providers.
type Catalog struct {
	Providers []Provider `yaml:"providers" json:"providers"`
}

// Provider returns the provider called name.
func (c *Catalog) Provider(name string) (Provider, bool) {
	i := slices.IndexFunc(c.Providers, func(p Provider) bool { return p.Name == name })
	if i < 0 {
		return Provider{}, false
	}
	return c.Providers[i], true
}

// Validate checks every provider as a harness's spec.model would be.
func (c *Catalog) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if len(c.Providers) > MaxProviders {
		add("at most %d providers", MaxProviders)
	}
	seen := map[string]bool{}
	for _, p := range c.Providers {
		what := "provider " + p.Name
		switch {
		case !nameRe.MatchString(p.Name):
			add("provider name %q must be 1-63 lowercase letters, digits or '-'", p.Name)
		case seen[p.Name]:
			add("%s: there is another provider with that name", what)
		}
		seen[p.Name] = true
		if !slices.Contains(providerTypes, p.Type) {
			add("%s: unknown type %q", what, p.Type)
		}
		m := spec.Model{Name: "model", Endpoint: p.URL, Credential: p.Credential}
		if err := spec.ValidateModel(&m, p.Private()); err != nil {
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

func (s *Server) modelsPath() string { return filepath.Join(filepath.Dir(s.cfgPath), "models.yaml") }

// LoadCatalog reads a catalog file; a missing one is empty. A catalog of
// the first format (endpoints and models) becomes providers.
func LoadCatalog(path string) (*Catalog, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the user's own file
	if errors.Is(err, os.ErrNotExist) {
		return &Catalog{}, nil
	}
	if err != nil {
		return nil, err
	}
	var file struct {
		Providers []Provider `yaml:"providers"`
		Endpoints []struct {
			Name, Kind, URL, Credential string
		} `yaml:"endpoints"`
		Models []yaml.Node `yaml:"models"`
	}
	if len(bytes.TrimSpace(data)) > 0 {
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&file); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	c := Catalog{Providers: file.Providers}
	for _, e := range file.Endpoints {
		t := "other"
		if e.Kind == "anthropic" {
			t = "anthropic"
		}
		c.Providers = append(c.Providers, Provider{Name: e.Name, Type: t, URL: e.URL, Credential: e.Credential})
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// ProviderView is a provider as the UI shows it.
type ProviderView struct {
	Provider
	Private bool `json:"private"`
	// UsedBy are the saved harnesses whose model is at this provider.
	UsedBy []string `json:"usedBy"`
}

func (s *Server) providersGet(w http.ResponseWriter, _ *http.Request) {
	s.modelsMu.Lock()
	c, err := LoadCatalog(s.modelsPath())
	s.modelsMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": s.providerViews(c)})
}

func (s *Server) providersPut(w http.ResponseWriter, r *http.Request) {
	var c Catalog
	if !readJSON(w, r, &c) {
		return
	}
	if err := c.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the providers are not valid", "errors": splitLines(err)})
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
	s.log.Info("model providers saved", "providers", len(c.Providers))
	writeJSON(w, http.StatusOK, map[string]any{"providers": s.providerViews(&c)})
}

func (s *Server) providerViews(c *Catalog) []ProviderView {
	out := make([]ProviderView, len(c.Providers))
	for i, p := range c.Providers {
		out[i] = ProviderView{Provider: p, Private: p.Private(), UsedBy: []string{}}
	}
	entries, err := s.lib.List()
	if err != nil {
		return out
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
		for i := range out {
			if strings.TrimSuffix(h.Spec.Model.Endpoint, "/") == strings.TrimSuffix(out[i].URL, "/") {
				out[i].UsedBy = append(out[i].UsedBy, e.Name)
			}
		}
	}
	return out
}

// ProviderModel is a model a provider offers.
type ProviderModel struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
}

// providerModels asks a provider which models it has.
func (s *Server) providerModels(w http.ResponseWriter, r *http.Request) {
	s.modelsMu.Lock()
	c, err := LoadCatalog(s.modelsPath())
	s.modelsMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	p, ok := c.Provider(r.PathValue("name"))
	if !ok {
		writeError(w, http.StatusNotFound, errNotFound("no provider called "+r.PathValue("name")))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), providerTimeout)
	defer cancel()
	models, err := s.fetchModels(ctx, p)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("asking %s for its models: %s", p.Name, safetext.Line(err.Error())))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

// fetchModels lists a provider's models: GET <url>/v1/models (Anthropic's
// and OpenAI-style APIs), or Ollama's /api/tags.
func (s *Server) fetchModels(ctx context.Context, p Provider) ([]ProviderModel, error) {
	header, value, err := s.providerAuth(p)
	if err != nil {
		return nil, err
	}
	return s.fetchModelsWith(ctx, p, header, value)
}

// fetchModelsWith lists p's models, sending header: value (if any).
func (s *Server) fetchModelsWith(ctx context.Context, p Provider, header, value string) ([]ProviderModel, error) {
	base := strings.TrimSuffix(p.URL, "/")
	get := func(path string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		if header != "" {
			req.Header.Set(header, value)
		}
		if p.Type == "anthropic" {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
		return s.providerClient.Do(req)
	}
	resp, err := get("/v1/models")
	if err == nil && resp.StatusCode == http.StatusNotFound && p.Type == "ollama" {
		_ = resp.Body.Close()
		resp, err = get("/api/tags")
	}
	if err != nil {
		return nil, whyUnreachable(p.URL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxModelsBody {
		return nil, errors.New("its answer is too large")
	}
	switch {
	case (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && header == "":
		return nil, fmt.Errorf("it wants a credential (%s): add its key on the Credentials page and choose it for this provider", resp.Status)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("it refused the credential (%s)", resp.Status)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("it answered %s", resp.Status)
	}
	var list struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, errors.New("its answer is not a model list")
	}
	var out []ProviderModel
	for _, d := range list.Data {
		out = append(out, ProviderModel{ID: d.ID, Label: d.DisplayName})
	}
	for _, m := range list.Models {
		out = append(out, ProviderModel{ID: m.Name})
	}
	// Only what a harness could use, once each.
	out = slices.DeleteFunc(out, func(m ProviderModel) bool {
		return spec.ValidateModel(&spec.Model{Name: m.ID, Endpoint: "https://a.example"}, false) != nil
	})
	slices.SortFunc(out, func(a, b ProviderModel) int { return strings.Compare(a.ID, b.ID) })
	out = slices.CompactFunc(out, func(a, b ProviderModel) bool { return a.ID == b.ID })
	for i := range out {
		out[i].Label = safetext.Line(out[i].Label)
	}
	if len(out) > maxProviderModels {
		out = out[:maxProviderModels]
	}
	if out == nil {
		out = []ProviderModel{}
	}
	return out, nil
}

// whyUnreachable says why a provider could not be reached, in words that
// point at the fix.
func whyUnreachable(rawURL string, err error) error {
	where := rawURL
	if _, host, port := (&spec.Model{Endpoint: rawURL}).EndpointHostPort(); host != "" {
		where = host + ":" + strconv.Itoa(port)
	}
	var dns *net.DNSError
	var ne net.Error
	switch {
	case errors.As(err, &dns):
		return fmt.Errorf("the name %s is not known: check the address", dns.Name)
	case errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("nothing is listening at %s: is the server running, and on this port?", where)
	case errors.As(err, &ne) && ne.Timeout():
		return fmt.Errorf("no answer from %s in time: is the address right, or is a firewall in the way?", where)
	default:
		return fmt.Errorf("cannot reach %s", where)
	}
}

// providerAuth is the header a provider's models are asked with: its
// credential from the store, but only if that credential may go to the
// provider's host and port (never anywhere else).
func (s *Server) providerAuth(p Provider) (header, value string, err error) {
	if p.Credential == "" {
		return "", "", nil
	}
	e, secret, held, err := s.storedCredential(p.Credential)
	if err != nil {
		return "", "", err
	}
	if !held {
		return "", "", fmt.Errorf("the credential %s is not on the Credentials page", p.Credential)
	}
	scheme, host, port := (&spec.Model{Endpoint: p.URL}).EndpointHostPort()
	if scheme != e.Scheme || port != e.Port || !slices.Contains(e.Hosts, host) {
		return "", "", fmt.Errorf("the credential %s is not for %s://%s:%d", p.Credential, scheme, host, port)
	}
	return e.Header, e.Prefix + secret, nil
}

// providerTestRequest is POST /api/providers/test: a connection as it
// is being edited, with a key typed for it (sent only to its own URL)
// or the name of a stored credential.
type providerTestRequest struct {
	Provider
	Auth *struct {
		Header string `json:"header"`
		Prefix string `json:"prefix"`
		Key    string `json:"key"`
	} `json:"auth,omitempty"`
}

var authHeaderRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// providerTest checks a connection before it is saved: does it answer,
// and with which models.
func (s *Server) providerTest(w http.ResponseWriter, r *http.Request) {
	var req providerTestRequest
	if !readJSON(w, r, &req) {
		return
	}
	defer func() {
		if req.Auth != nil {
			req.Auth.Key = ""
		}
	}()
	p := req.Provider
	if p.Name == "" {
		p.Name = "test"
	}
	c := Catalog{Providers: []Provider{p}}
	if err := c.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	header, value := "", ""
	switch {
	case req.Auth != nil && req.Auth.Key != "":
		if !authHeaderRe.MatchString(req.Auth.Header) || strings.ContainsAny(req.Auth.Prefix+req.Auth.Key, "\r\n\x00") {
			writeError(w, http.StatusBadRequest, errors.New("invalid header or key"))
			return
		}
		header, value = req.Auth.Header, req.Auth.Prefix+req.Auth.Key
	case p.Credential != "":
		var err error
		if header, value, err = s.providerAuth(p); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), providerTimeout)
	defer cancel()
	models, err := s.fetchModelsWith(ctx, p, header, value)
	if err != nil {
		writeError(w, http.StatusBadGateway, errors.New(safetext.Line(err.Error())))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

// newProviderClient asks providers: bounded in time, and never following
// a redirect (which could carry the credential elsewhere).
func newProviderClient() *http.Client {
	return &http.Client{
		Timeout:       providerTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
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
