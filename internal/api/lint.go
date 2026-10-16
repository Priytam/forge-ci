package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
)

// lintJob is one job of a validated config, as it would run on the given ref.
type lintJob struct {
	Name         string   `json:"name"`
	Stage        string   `json:"stage"`
	Environment  string   `json:"environment,omitempty"`
	Needs        []string `json:"needs"`
	Image        string   `json:"image,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	Manual       bool     `json:"manual,omitempty"`
	AllowFailure bool     `json:"allow_failure,omitempty"`
	// Refused is set when this server's environment policy would refuse the
	// job on this ref (branch not allowed, or unregistered environment).
	Refused string `json:"refused,omitempty"`
}

type lintResponse struct {
	Valid  bool      `json:"valid"`
	Error  string    `json:"error,omitempty"`
	Ref    string    `json:"ref"`
	Stages []string  `json:"stages"`
	Jobs   []lintJob `json:"jobs"`
}

// lint validates pipeline YAML without running anything: POST /api/v1/lint
// {config, ref?, repo?, source?}. It compiles exactly as a real run would
// (includes from repo's templates, rules evaluated for ref and source) and
// returns the resulting jobs, or the compile error. 200 either way; "valid"
// says which. Any signed-in user may call it.
func (s *Server) lint(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Config string `json:"config"`
		Ref    string `json:"ref"`
		Repo   string `json:"repo"`
		Source string `json:"source"`
		// ChangedFiles, when given, previews rules: changes: as if these files
		// changed; omitted = unknown (every changes: matches).
		ChangedFiles []string `json:"changed_files"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body (want {config, ref, repo})")
		return
	}
	if strings.TrimSpace(req.Config) == "" {
		writeErr(w, http.StatusBadRequest, "config is required")
		return
	}
	if req.Ref == "" {
		req.Ref = "main"
	}
	source := compiler.SourceAPI
	if req.Source != "" {
		source = req.Source
	}
	resp := lintResponse{Ref: req.Ref, Stages: []string{}, Jobs: []lintJob{}}
	var changes *compiler.ChangeSet
	if req.ChangedFiles != nil {
		changes = &compiler.ChangeSet{Files: req.ChangedFiles}
	}
	jobs, err := compiler.CompileWithChanges(req.Config, req.Ref, source, s.store.TemplateResolver(r.Context(), req.Repo), changes)
	if err == nil {
		_, err = compiler.Options(req.Config)
	}
	if err != nil {
		resp.Error = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	policy, err := s.store.GetEnvPolicy(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load environment policy")
		return
	}
	seen := map[string]bool{}
	for _, j := range jobs {
		if !seen[j.Stage] {
			seen[j.Stage] = true
			resp.Stages = append(resp.Stages, j.Stage)
		}
		env, refused := policy.CheckJob(j.Environment, req.Ref)
		needs := j.Needs
		if needs == nil {
			needs = []string{}
		}
		resp.Jobs = append(resp.Jobs, lintJob{
			Name: j.Name, Stage: j.Stage, Environment: env, Needs: needs, Image: j.Image,
			Tags: j.Tags, Manual: j.Manual, AllowFailure: j.AllowFailure, Refused: refused,
		})
	}
	resp.Valid = true
	writeJSON(w, http.StatusOK, resp)
}
