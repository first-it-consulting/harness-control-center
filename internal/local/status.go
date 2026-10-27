package local

import (
	"context"
	"time"

	"github.com/first-it-consulting/harness-control-center/internal/runnerd"
)

// Status is the local runner's state, for "hcc local status" and the
// Control Center.
type Status struct {
	// Supported is empty on a Mac with Apple Silicon, else why not.
	Supported string `json:"supported,omitempty"`
	// Images: the VM images are in place.
	Images bool `json:"images"`
	// ClaudeCode is the version added to the claude-code image, if hcc
	// added it (images built on this Mac bring their own).
	ClaudeCode string `json:"claudeCode,omitempty"`
	Config     bool   `json:"config"`
	// Service: launchd has the runner's agent loaded.
	Service bool `json:"service"`
	// Answering: the runner answers on its socket.
	Answering bool   `json:"answering"`
	Socket    string `json:"socket"`
}

// Ready reports whether agents can run here.
func (s Status) Ready() bool { return s.Supported == "" && s.Images && s.Config && s.Answering }

// GetStatus looks at the layout, the service and the socket.
func GetStatus(ctx context.Context, l Layout, svc Service) Status {
	st := Status{Socket: l.Socket()}
	if err := Supported(); err != nil {
		st.Supported = err.Error()
		return st
	}
	st.Images = exists(l.kernel()) && exists(l.baseRootFS()) && exists(l.claudeRootFS())
	st.ClaudeCode = l.readInstalled().ClaudeCode
	st.Config = exists(l.Config())
	st.Service = svc.Running(ctx)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := runnerd.NewUnixClient(l.Socket()).Info(ctx)
	st.Answering = err == nil
	return st
}
