package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/first-it-consulting/harness-control-center/internal/credentials"
	"github.com/first-it-consulting/harness-control-center/internal/runnerd"
)

// The Control Center's own credential store (ADR-0019): the person keeps
// their credentials here, once, and attaches them to harnesses. When a
// harness starts (or is scheduled) on a runner, the credentials it is
// granted are copied into that runner's broker store (ADR-0017), which
// adds them to the agent's requests; the agent never sees them.
//
// The store is <dir>/credentials/credentials.yaml with one secret file
// each in secrets/, owner-only, like the runners' stores. copies.yaml
// records which version of each credential each runner has (a hash of
// the entry and secret), so an unchanged credential is not copied again.

func (s *Server) storePaths() (store, secrets, copies string) {
	dir := filepath.Join(filepath.Dir(s.cfgPath), "credentials")
	return filepath.Join(dir, "credentials.yaml"), filepath.Join(dir, "secrets"), filepath.Join(dir, "copies.yaml")
}

// ensureStore creates an empty, owner-only store.
func (s *Server) ensureStore() error {
	store, _, _ := s.storePaths()
	if err := os.MkdirAll(filepath.Dir(store), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(store, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the user's own store
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = f.WriteString("# The Control Center's credentials; secrets are in secrets/.\n{}\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// copies is runner → credential → fingerprint of what it was given.
type copies map[string]map[string]string

func (s *Server) loadCopies() copies {
	_, _, path := s.storePaths()
	c := copies{}
	if data, err := os.ReadFile(path); err == nil { //nolint:gosec // the user's own file
		_ = yaml.Unmarshal(data, &c)
	}
	return c
}

func (s *Server) saveCopies(c copies) error {
	_, _, path := s.storePaths()
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

// fingerprint identifies a credential's entry and secret together.
func fingerprint(e credentials.Entry, secret string) string {
	b, _ := json.Marshal(e) //nolint:errchkjson // plain struct
	h := sha256.New()
	h.Write(b)
	h.Write([]byte{0})
	h.Write([]byte(secret))
	return hex.EncodeToString(h.Sum(nil))
}

// storedCredential returns a credential of the store with its secret.
func (s *Server) storedCredential(name string) (credentials.Entry, string, bool, error) {
	store, _, _ := s.storePaths()
	if _, err := os.Stat(store); errors.Is(err, os.ErrNotExist) {
		return credentials.Entry{}, "", false, nil
	}
	entries, err := credentials.List(store)
	if err != nil {
		return credentials.Entry{}, "", false, err
	}
	i := slices.IndexFunc(entries, func(e credentials.Entry) bool { return e.Name == name })
	if i < 0 {
		return credentials.Entry{}, "", false, nil
	}
	all, err := credentials.Load(store)
	if err != nil {
		return credentials.Entry{}, "", false, err
	}
	e := entries[i]
	if e.Kind == credentials.KindSecret {
		return e, all[name].Value, true, nil
	}
	return e, strings.TrimPrefix(all[name].Value, e.Prefix), true, nil
}

// CredentialView is a stored credential as the UI lists it: never its
// secret, and on which runners it is current.
type CredentialView struct {
	credentials.Entry
	// Runners maps a runner to "current" or "outdated" (it has an
	// older version) for each runner it was copied to.
	Runners map[string]string `json:"runners"`
}

func (s *Server) credentialsList(w http.ResponseWriter, _ *http.Request) {
	s.credMu.Lock()
	defer s.credMu.Unlock()
	store, _, _ := s.storePaths()
	out := []CredentialView{}
	if _, err := os.Stat(store); err == nil {
		entries, err := credentials.List(store)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		cp := s.loadCopies()
		all, _ := credentials.Load(store)
		for _, e := range entries {
			v := CredentialView{Entry: e, Runners: map[string]string{}}
			fp := fingerprint(e, strings.TrimPrefix(all[e.Name].Value, e.Prefix))
			for runner, have := range cp {
				if f, ok := have[e.Name]; ok {
					v.Runners[runner] = map[bool]string{true: "current", false: "outdated"}[f == fp]
				}
			}
			out = append(out, v)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": out})
}

func (s *Server) credentialStore(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req credentialChange
	if !readJSON(w, r, &req) {
		return
	}
	defer func() {
		if req.Secret != nil {
			*req.Secret = ""
		}
	}()
	s.credMu.Lock()
	defer s.credMu.Unlock()
	if err := s.ensureStore(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	store, secrets, _ := s.storePaths()
	e := credentials.Entry{Name: name, Kind: req.Kind, Hosts: req.Hosts, Scheme: req.Scheme, Port: req.Port, Header: req.Header, Prefix: req.Prefix}
	var secret []byte
	if req.Secret != nil {
		secret = []byte(*req.Secret)
		defer clear(secret)
	}
	if err := credentials.Set(store, secrets, e, secret); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.log.Info("credential stored", "credential", name)
	writeJSON(w, http.StatusOK, map[string]string{"credential": name})
}

func (s *Server) credentialDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.credMu.Lock()
	defer s.credMu.Unlock()
	store, secrets, _ := s.storePaths()
	if err := credentials.Remove(store, secrets, name); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// The runners keep their copies until removed there (their broker
	// may be serving a running job); the UI offers that.
	s.log.Info("credential deleted", "credential", name)
	writeJSON(w, http.StatusOK, map[string]string{"credential": name})
}

// copyRequest is POST /api/credentials/{name}/copy.
type copyRequest struct {
	Runner       string  `json:"runner"`
	SudoPassword *string `json:"sudoPassword,omitempty"`
}

// credentialCopy copies one stored credential to a runner now (for
// runners that have an older version).
func (s *Server) credentialCopy(w http.ResponseWriter, r *http.Request) {
	var req copyRequest
	if !readJSON(w, r, &req) {
		return
	}
	var pw []byte
	if req.SudoPassword != nil {
		pw = []byte(*req.SudoPassword)
		*req.SudoPassword = ""
		defer clear(pw)
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	err := s.copyCredentials(ctx, req.Runner, []string{r.PathValue("name")}, pw, true)
	writeCopyResult(w, err, map[string]string{"credential": r.PathValue("name"), "runner": req.Runner})
}

func writeCopyResult(w http.ResponseWriter, err error, ok any) {
	switch {
	case errors.Is(err, runnerd.ErrSudoPassword), errors.Is(err, runnerd.ErrSudoRefused):
		writeJSON(w, http.StatusForbidden, map[string]any{"error": err.Error(), "needPassword": true})
	case errors.Is(err, errNotFoundSentinel):
		writeError(w, http.StatusNotFound, err)
	case err != nil:
		writeError(w, http.StatusBadRequest, err)
	default:
		writeJSON(w, http.StatusOK, ok)
	}
}

var errNotFoundSentinel = errors.New("not found")

// copyCredentials makes sure the runner's broker has the current version
// of each named credential this store holds; names the store does not
// hold are left to the runner (it may have them from before). Unless
// force is set, a credential the runner has in the same version is not
// copied again. pw is the sudo password for a runner over ssh, if any.
func (s *Server) copyCredentials(ctx context.Context, runner string, names []string, pw []byte, force bool) error {
	// Names come from requests: only well-formed ones go further (into
	// paths, commands and the log).
	if !nameRe.MatchString(runner) {
		return fmt.Errorf("%w: invalid runner name", errNotFoundSentinel)
	}
	for _, name := range names {
		if !nameRe.MatchString(name) {
			return fmt.Errorf("invalid credential name %q", name)
		}
	}
	s.credMu.Lock()
	defer s.credMu.Unlock()
	ref, ok := s.config().Runner(runner)
	if !ok {
		return fmt.Errorf("%w: no runner called %s", errNotFoundSentinel, runner)
	}
	cp := s.loadCopies()
	var onRunner []credentials.Entry
	listed := false
	for _, name := range names {
		e, secret, held, err := s.storedCredential(name)
		if err != nil {
			return err
		}
		if !held {
			continue
		}
		fp := fingerprint(e, secret)
		if !force && cp[runner][name] == fp {
			// Recorded as current; check the runner still lists it.
			if !listed {
				onRunner = s.runnerCredentials(ctx, runner)
				listed = true
			}
			if slices.ContainsFunc(onRunner, func(x credentials.Entry) bool { return x.Name == name }) {
				continue
			}
		}
		req := credentialChange{Kind: e.Kind, Hosts: e.Hosts, Scheme: e.Scheme, Port: e.Port, Header: e.Header, Prefix: e.Prefix, Secret: &secret}
		if pw != nil {
			p := string(pw)
			req.SudoPassword = &p
		}
		if ref.SSH != "" {
			err = s.changeOverSSH(ctx, ref.SSH, e, req, false)
		} else {
			err = s.changeHere(ctx, runner, e, &secret, false)
		}
		if err != nil {
			return fmt.Errorf("copying %s to %s: %w", name, runner, err)
		}
		if cp[runner] == nil {
			cp[runner] = map[string]string{}
		}
		cp[runner][name] = fp
		if err := s.saveCopies(cp); err != nil {
			return err
		}
		s.log.Info("credential copied to runner", "credential", e.Name, "runner", ref.Name)
	}
	return nil
}

// runnerCredentials lists a runner's broker store (nil if unknown).
func (s *Server) runnerCredentials(ctx context.Context, runner string) []credentials.Entry {
	c, err := s.client(runner)
	if err != nil {
		return nil
	}
	l, err := c.Credentials(ctx)
	if err != nil {
		return nil
	}
	return l.Credentials
}
