package main

import (
	"strings"
	"testing"

	"github.com/first-it-consulting/harness-control-center/internal/credentials"
	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

func TestBrokerModel(t *testing.T) {
	llm := credentials.Credential{Name: "llm", Hosts: []string{"llm.example"}, Scheme: "http", Port: 4000, Header: "x-api-key", Value: "s"}
	api := credentials.Credential{Name: "api", Hosts: []string{"api.example"}, Scheme: "https", Port: 443, Header: "x-api-key", Value: "s"}
	tests := map[string]struct {
		have    map[string]credentials.Credential
		model   spec.Model
		wantErr string
		metered bool // a credential-less entry was added
	}{
		"https without credential is metered": {map[string]credentials.Credential{}, spec.Model{Name: "m", Endpoint: "https://open.example"}, "", true},
		"matching credential":                 {map[string]credentials.Credential{"llm.example": llm}, spec.Model{Name: "m", Endpoint: "http://llm.example:4000", Credential: "llm"}, "", false},
		"http needs a credential":             {map[string]credentials.Credential{}, spec.Model{Name: "m", Endpoint: "http://llm.example:4000"}, "needs a credential", false},
		"port mismatch":                       {map[string]credentials.Credential{"llm.example": llm}, spec.Model{Name: "m", Endpoint: "http://llm.example:5000", Credential: "llm"}, "does not match", false},
		"scheme mismatch":                     {map[string]credentials.Credential{"api.example": api}, spec.Model{Name: "m", Endpoint: "http://api.example:443", Credential: "api"}, "does not match", false},
		"credential for another host":         {map[string]credentials.Credential{"api.example": api}, spec.Model{Name: "m", Endpoint: "https://open.example", Credential: "api"}, "is not for the model endpoint", false},
		"host covered by another credential":  {map[string]credentials.Credential{"api.example": api}, spec.Model{Name: "m", Endpoint: "https://api.example", Credential: "other"}, "not \"other\"", false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := brokerModel(tt.have, &tt.model)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			_, host, _ := tt.model.EndpointHostPort()
			c, ok := tt.have[host]
			if !ok {
				t.Fatal("model endpoint not brokered")
			}
			if tt.metered && (c.Header != "" || c.Value != "") {
				t.Errorf("metering-only entry carries a credential: %v", c)
			}
		})
	}
}
