package runnerd

import (
	"context"
	"net/http"

	"github.com/first-it-consulting/harness-control-center/internal/credentials"
)

// CredentialList is what GET /v1/credentials answers: the credentials
// this runner can broker, never their secrets (ADR-0017).
type CredentialList struct {
	// Store is the store file, empty if the runner has none configured.
	Store       string              `json:"store,omitempty"`
	Credentials []credentials.Entry `json:"credentials"`
	// Error says why the store could not be read.
	Error string `json:"error,omitempty"`
}

// EnableCredentials serves the list of the credential store at path
// (empty: the runner has none).
func (s *Server) EnableCredentials(path string) {
	s.mux.HandleFunc("GET /v1/credentials", func(w http.ResponseWriter, _ *http.Request) {
		out := CredentialList{Store: path, Credentials: []credentials.Entry{}}
		if path != "" {
			list, err := credentials.List(path)
			if err != nil {
				// The runner checks the store for every job anyway; this only
				// reports why it is unusable (Load's errors name no secret).
				out.Error = err.Error()
			} else {
				out.Credentials = list
			}
		}
		writeJSON(w, http.StatusOK, out)
	})
}

// Credentials lists the runner's credentials, without their secrets.
func (c *Client) Credentials(ctx context.Context) (CredentialList, error) {
	var l CredentialList
	return l, c.do(ctx, http.MethodGet, "/v1/credentials", nil, &l)
}
