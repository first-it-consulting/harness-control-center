package runnerd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/plugins"
)

// EnablePlugins serves the plugin routes (ADR-0013). Call it before Serve.
func (s *Server) EnablePlugins(store *plugins.Store, mgr *plugins.Manager) {
	s.plugins, s.manager = store, mgr
	s.mux.HandleFunc("GET /v1/plugins", s.listPlugins)
	s.mux.HandleFunc("POST /v1/plugins", s.installPlugin)
	s.mux.HandleFunc("DELETE /v1/plugins/{name}", s.removePlugin)
	s.mux.HandleFunc("GET /v1/plugins/{name}/log", s.pluginLog)
}

// PluginView is an installed plugin with its activity.
type PluginView struct {
	plugins.Installed
	Status plugins.Status `json:"status"`
}

func (s *Server) listPlugins(w http.ResponseWriter, _ *http.Request) {
	list, err := s.plugins.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	status := map[string]plugins.Status{}
	for _, st := range s.manager.Statuses() {
		status[st.Name] = st
	}
	out := make([]PluginView, 0, len(list))
	for _, in := range list {
		out = append(out, PluginView{Installed: in, Status: status[in.Name]})
	}
	writeJSON(w, http.StatusOK, out)
}

// installPlugin previews a bundle; with ?confirm=<its checksum> it
// installs exactly that bundle.
func (s *Server) installPlugin(w http.ResponseWriter, req *http.Request) {
	// A bundle may take a while over ssh.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(5 * time.Minute))
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, plugins.MaxBundleBytes+1<<20))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	confirm := req.URL.Query().Get("confirm")
	if confirm == "" {
		_, _, pv, err := plugins.Check(bytes.NewReader(body))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, pv)
		return
	}
	in, err := s.plugins.Install(bytes.NewReader(body), confirm)
	switch {
	case errors.Is(err, plugins.ErrInstalled):
		writeError(w, http.StatusConflict, err)
	case err != nil:
		writeError(w, http.StatusBadRequest, err)
	default:
		s.log.Warn("plugin installed", "plugin", in.Name, "version", in.Version, "type", in.Type, "sha256", in.SHA256, "risk", in.Risk)
		writeJSON(w, http.StatusCreated, in)
	}
}

func (s *Server) removePlugin(w http.ResponseWriter, req *http.Request) {
	name := req.PathValue("name")
	err := s.plugins.Remove(name)
	switch {
	case errors.Is(err, plugins.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case err != nil:
		writeError(w, http.StatusBadRequest, err)
	default:
		s.log.Warn("plugin removed", "plugin", name)
		writeJSON(w, http.StatusOK, map[string]string{"removed": name})
	}
}

func (s *Server) pluginLog(w http.ResponseWriter, req *http.Request) {
	name := req.PathValue("name")
	var id string
	for _, st := range s.manager.Statuses() {
		if st.Name == name {
			id = st.LastRunID
		}
	}
	if id == "" {
		writeError(w, http.StatusNotFound, errors.New("the plugin has not run since the runner started"))
		return
	}
	p, err := consolePath(s.stateDir, id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	f, err := os.Open(p) //nolint:gosec // path built from a validated id
	if err != nil {
		writeError(w, http.StatusNotFound, errors.New("the log is gone"))
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.Copy(w, io.LimitReader(f, MaxConsoleBytes+int64(len(truncatedMarker))))
}

// Plugins lists the runner's plugins.
func (c *Client) Plugins(ctx context.Context) ([]PluginView, error) {
	var out []PluginView
	return out, c.do(ctx, http.MethodGet, "/v1/plugins", nil, &out)
}

// PreviewPlugin returns what installing a bundle means.
func (c *Client) PreviewPlugin(ctx context.Context, bundle []byte) (plugins.Preview, error) {
	var pv plugins.Preview
	return pv, c.do(ctx, http.MethodPost, "/v1/plugins", bytes.NewReader(bundle), &pv)
}

// InstallPlugin installs a bundle whose preview's checksum was confirmed.
func (c *Client) InstallPlugin(ctx context.Context, bundle []byte, sha256 string) (plugins.Installed, error) {
	var in plugins.Installed
	return in, c.do(ctx, http.MethodPost, "/v1/plugins?confirm="+url.QueryEscape(sha256), bytes.NewReader(bundle), &in)
}

// RemovePlugin uninstalls a plugin.
func (c *Client) RemovePlugin(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/v1/plugins/"+url.PathEscape(name), nil, nil)
}

// PluginLog copies a plugin's last run's console to w.
func (c *Client) PluginLog(ctx context.Context, name string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/plugins/"+url.PathEscape(name)+"/log", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &APIError{Status: resp.StatusCode, Message: resp.Status}
	}
	_, err = io.Copy(w, io.LimitReader(resp.Body, MaxConsoleBytes+1<<10))
	return err
}
