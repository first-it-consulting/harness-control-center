//go:build unix

package runnerd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/first-it-consulting/harness-control-center/internal/credentials"
)

func TestCredentialListHasNoSecrets(t *testing.T) {
	dir := shortTempDir(t)
	d := startDaemonIn(t, dir)
	store := filepath.Join(dir, "credentials.yaml")
	// No store file yet: reported, not fatal.
	l, err := d.client.Credentials(context.Background())
	if err != nil || l.Error == "" || len(l.Credentials) != 0 {
		t.Fatalf("without a store: %+v %v", l, err)
	}
	if err := os.WriteFile(store, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := credentials.Entry{Name: "anthropic", Hosts: []string{"api.anthropic.com"}, Header: "x-api-key"}
	if err := credentials.Set(store, filepath.Join(dir, "secrets"), e, []byte("sk-ant-very-secret")); err != nil {
		t.Fatal(err)
	}
	l, err = d.client.Credentials(context.Background())
	if err != nil || l.Error != "" || len(l.Credentials) != 1 || l.Credentials[0].Name != "anthropic" || l.Credentials[0].Port != 443 {
		t.Fatalf("got %+v %v", l, err)
	}
	raw, _ := json.Marshal(l)
	if strings.Contains(string(raw), "sk-ant") || strings.Contains(string(raw), "secrets/") {
		t.Errorf("the list shows a secret or its file: %s", raw)
	}
}
