package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/credentials"
	"github.com/first-it-consulting/harness-control-center/internal/runnerd"
)

// Credentials (ADR-0017): listed through the runner, without secrets;
// changed where the store is, as its owner. A secret passes through here
// once, on its way, and is never stored, logged or sent back.

// credentialChange is a PUT or DELETE body.
type credentialChange struct {
	Hosts  []string `json:"hosts"`
	Scheme string   `json:"scheme"`
	Port   int      `json:"port"`
	Header string   `json:"header"`
	Prefix string   `json:"prefix"`
	// Secret, if set, replaces the secret; without it the secret stays.
	Secret *string `json:"secret,omitempty"`
	// SudoPassword is for a runner reached over ssh whose sudo needs it.
	SudoPassword *string `json:"sudoPassword,omitempty"`
}

// The runner's configuration on a server (install-runner.sh).
const remoteConfig = "/etc/hcc/runner.yaml"

// adminTimeout bounds a change over ssh.
const adminTimeout = 60 * time.Second

func (s *Server) credentialList(w http.ResponseWriter, r *http.Request) {
	c, err := s.client(r.PathValue("runner"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), runnerTimeout)
	defer cancel()
	l, err := c.Credentials(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	// Where the store is matters to nobody in the UI.
	l.Store = ""
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) credentialSet(w http.ResponseWriter, r *http.Request) {
	s.changeCredential(w, r, false)
}

func (s *Server) credentialRemove(w http.ResponseWriter, r *http.Request) {
	s.changeCredential(w, r, true)
}

func (s *Server) changeCredential(w http.ResponseWriter, r *http.Request, remove bool) {
	runner, name := r.PathValue("runner"), r.PathValue("name")
	if !nameRe.MatchString(name) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("a credential's name is 1-63 lowercase letters, digits or '-', got %q", name))
		return
	}
	var req credentialChange
	if !readJSON(w, r, &req) {
		return
	}
	defer func() {
		if req.Secret != nil {
			*req.Secret = ""
		}
		if req.SudoPassword != nil {
			*req.SudoPassword = ""
		}
	}()
	s.mu.Lock()
	ref, ok := s.cfg.Runner(runner)
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, errNotFound("no runner called "+runner))
		return
	}
	entry := credentials.Entry{Name: name, Hosts: req.Hosts, Scheme: req.Scheme, Port: req.Port, Header: req.Header, Prefix: req.Prefix}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	var err error
	if ref.SSH != "" {
		err = s.changeOverSSH(ctx, ref.SSH, entry, req, remove)
	} else {
		err = s.changeHere(ctx, runner, entry, req.Secret, remove)
	}
	switch {
	case errors.Is(err, runnerd.ErrSudoPassword), errors.Is(err, runnerd.ErrSudoRefused):
		writeJSON(w, http.StatusForbidden, map[string]any{"error": err.Error(), "needPassword": true})
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, err)
		return
	}
	what := "saved"
	if remove {
		what = "removed"
	}
	s.log.Info("credential "+what, "runner", runner, "credential", name)
	writeJSON(w, http.StatusOK, map[string]string{"credential": name})
}

// changeOverSSH runs "hcc credentials" with sudo on the runner's host.
func (s *Server) changeOverSSH(ctx context.Context, host string, e credentials.Entry, req credentialChange, remove bool) error {
	var pw []byte
	if req.SudoPassword != nil {
		pw = []byte(*req.SudoPassword)
		defer clear(pw)
	}
	if remove {
		_, err := runnerd.SudoSSH(ctx, host, pw, []string{"credentials", "remove", "--config", remoteConfig, e.Name}, nil)
		return err
	}
	// The secret is meant to go here: to the runner's host, on stdin.
	body, err := json.Marshal(struct { //nolint:gosec // G117: see above
		credentials.Entry
		Secret *string `json:"secret,omitempty"`
	}{e, req.Secret})
	if err != nil {
		return err
	}
	defer clear(body)
	_, err = runnerd.SudoSSH(ctx, host, pw, []string{"credentials", "set", "--config", remoteConfig}, body)
	return err
}

// changeHere edits the store of a runner on this computer, which belongs
// to this user (the Mac's runner).
func (s *Server) changeHere(ctx context.Context, runner string, e credentials.Entry, secret *string, remove bool) error {
	c, err := s.client(runner)
	if err != nil {
		return err
	}
	l, err := c.Credentials(ctx)
	if err != nil {
		return err
	}
	if l.Store == "" {
		return errors.New("this runner has no credential store (credentials: in its runner.yaml)")
	}
	secrets := filepath.Join(filepath.Dir(l.Store), "secrets")
	if remove {
		return credentials.Remove(l.Store, secrets, e.Name)
	}
	var b []byte
	if secret != nil {
		b = []byte(*secret)
		defer clear(b)
	}
	return credentials.Set(l.Store, secrets, e, b)
}
