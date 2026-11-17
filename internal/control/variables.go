package control

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/first-it-consulting/harness-control-center/internal/spec"
)

// Variables: values that are not secret (a region, a database host), kept
// by the Control Center and attached to harnesses, which get a copy in
// spec.env. Unlike secrets they are shown, here and in the harness.

// MaxVariables bounds the variables kept.
const MaxVariables = 200

// Variable is one variable.
type Variable struct {
	Name    string    `yaml:"name" json:"name"`
	Value   string    `yaml:"value" json:"value"`
	Updated time.Time `yaml:"updated" json:"updated"`
}

type variablesFile struct {
	Variables []Variable `yaml:"variables"`
}

func (s *Server) variablesPath() string {
	return filepath.Join(filepath.Dir(s.cfgPath), "variables.yaml")
}

func (s *Server) loadVariables() ([]Variable, error) {
	data, err := os.ReadFile(s.variablesPath())
	if errors.Is(err, os.ErrNotExist) {
		return []Variable{}, nil
	}
	if err != nil {
		return nil, err
	}
	var f variablesFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", s.variablesPath(), err)
	}
	if f.Variables == nil {
		f.Variables = []Variable{}
	}
	return f.Variables, nil
}

func checkVariable(name, value string) error {
	if err := spec.CheckEnvName(name); err != nil {
		return err
	}
	if len(value) > spec.MaxEnvValueBytes || strings.ContainsRune(value, 0) {
		return fmt.Errorf("the value must be text of at most %d bytes", spec.MaxEnvValueBytes)
	}
	return nil
}

func (s *Server) variablesGet(w http.ResponseWriter, _ *http.Request) {
	s.modelsMu.Lock()
	vs, err := s.loadVariables()
	s.modelsMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"variables": vs})
}

type variableChange struct {
	Value string `json:"value"`
}

func (s *Server) variablePut(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req variableChange
	if !readJSON(w, r, &req) {
		return
	}
	if err := checkVariable(name, req.Value); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.modelsMu.Lock()
	defer s.modelsMu.Unlock()
	vs, err := s.loadVariables()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	v := Variable{Name: name, Value: req.Value, Updated: time.Now().UTC().Truncate(time.Second)}
	if i := slices.IndexFunc(vs, func(x Variable) bool { return x.Name == name }); i >= 0 {
		vs[i] = v
	} else {
		if len(vs) >= MaxVariables {
			writeError(w, http.StatusBadRequest, fmt.Errorf("at most %d variables", MaxVariables))
			return
		}
		vs = append(vs, v)
	}
	slices.SortFunc(vs, func(a, b Variable) int { return strings.Compare(a.Name, b.Name) })
	if err := s.saveVariables(vs); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) variableDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.modelsMu.Lock()
	defer s.modelsMu.Unlock()
	vs, err := s.loadVariables()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	i := slices.IndexFunc(vs, func(x Variable) bool { return x.Name == name })
	if i < 0 {
		writeError(w, http.StatusNotFound, errNotFound("no variable called "+name))
		return
	}
	if err := s.saveVariables(slices.Delete(vs, i, i+1)); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"variable": name})
}

func (s *Server) saveVariables(vs []Variable) error {
	data, err := yaml.Marshal(variablesFile{Variables: vs})
	if err != nil {
		return err
	}
	return writeFileAtomic(s.variablesPath(), data)
}
