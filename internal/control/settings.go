package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/first-it-consulting/harness-control-center/internal/runnerd"
)

// A runner's settings (maxConcurrent, retention, timeouts, pricing …):
// read from the runner, changed where its runner.yaml is, as the file's
// owner, within the bounds the runner's host reports. Changing them
// restarts the runner, so it is refused while jobs run or wait.

// settingsChange is a PUT body.
type settingsChange struct {
	Settings runnerd.Settings `json:"settings"`
	// SudoPassword is for a runner reached over ssh whose sudo needs it.
	SudoPassword *string `json:"sudoPassword,omitempty"`
}

func (s *Server) settingsGet(w http.ResponseWriter, r *http.Request) {
	c, err := s.client(r.PathValue("runner"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), runnerTimeout)
	defer cancel()
	v, err := c.Settings(ctx)
	if errors.Is(err, runnerd.ErrNoSettings) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "needUpdate": true})
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	// Where the file is matters to nobody in the UI.
	v.Config = ""
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) settingsSet(w http.ResponseWriter, r *http.Request) {
	runner := r.PathValue("runner")
	var req settingsChange
	if !readJSON(w, r, &req) {
		return
	}
	defer func() {
		if req.SudoPassword != nil {
			*req.SudoPassword = ""
		}
	}()
	ref, ok := s.config().Runner(runner)
	if !ok {
		writeError(w, http.StatusNotFound, errNotFound("no runner called "+runner))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adminTimeout)
	defer cancel()
	// Applying the settings restarts the runner, which would end its jobs.
	n, err := s.activeJobs(ctx, runner)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if n > 0 {
		writeError(w, http.StatusConflict, fmt.Errorf("%s has %d jobs running or queued: changing its settings restarts it and would end them. Wait until they finish, or stop them first", runner, n))
		return
	}
	if ref.SSH != "" {
		err = s.settingsOverSSH(ctx, ref.SSH, req)
	} else {
		err = s.settingsHere(ctx, runner, req.Settings)
	}
	switch {
	case errors.Is(err, runnerd.ErrSudoPassword), errors.Is(err, runnerd.ErrSudoRefused):
		writeJSON(w, http.StatusForbidden, map[string]any{"error": err.Error(), "needPassword": true})
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.log.Info("runner settings changed", "runner", runner)
	writeJSON(w, http.StatusOK, map[string]string{"runner": runner})
}

// settingsOverSSH runs "hcc runner settings set" with sudo on the
// runner's host, which checks the bounds there and restarts the runner.
func (s *Server) settingsOverSSH(ctx context.Context, host string, req settingsChange) error {
	var pw []byte
	if req.SudoPassword != nil {
		pw = []byte(*req.SudoPassword)
		defer clear(pw)
	}
	body, err := json.Marshal(req.Settings)
	if err != nil {
		return err
	}
	_, err = runnerd.SudoSSH(ctx, host, pw, []string{"runner", "settings", "set", "--config", remoteConfig, "--restart"}, body)
	return err
}

// settingsHere changes the settings of a runner on this computer, whose
// configuration belongs to this user (the Mac's runner), and restarts it.
func (s *Server) settingsHere(ctx context.Context, runner string, settings runnerd.Settings) error {
	c, err := s.client(runner)
	if err != nil {
		return err
	}
	v, err := c.Settings(ctx)
	if err != nil {
		return err
	}
	// Only the Mac's own runner: a socket's answer does not choose which
	// file this user's Control Center rewrites.
	if v.Config == "" || v.Config != s.local.config {
		return errors.New("only the settings of the runner on this computer can be changed here; change the others on their own host")
	}
	if err := runnerd.ApplySettings(v.Config, settings, runnerd.HostBounds()); err != nil {
		return err
	}
	if err := s.local.restart(ctx); err != nil {
		return fmt.Errorf("the settings are saved, but restarting the runner failed (restart it to apply them): %w", err)
	}
	return nil
}
