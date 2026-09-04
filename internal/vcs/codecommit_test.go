package vcs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseCodeCommitURL(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		want   CodeCommitRepo
		wantOK bool
	}{
		{
			name:   "git-remote-codecommit form",
			raw:    "codecommit::ap-south-1://tablespace-api",
			want:   CodeCommitRepo{Region: "ap-south-1", Name: "tablespace-api"},
			wantOK: true,
		},
		{
			name:   "git-remote-codecommit with named profile",
			raw:    "codecommit::us-east-1://prod@tablespace-api",
			want:   CodeCommitRepo{Region: "us-east-1", Name: "tablespace-api", Profile: "prod"},
			wantOK: true,
		},
		{
			name:   "GRC HTTPS form",
			raw:    "https://git-codecommit.ap-south-1.amazonaws.com/v1/repos/tablespace-api",
			want:   CodeCommitRepo{Region: "ap-south-1", Name: "tablespace-api"},
			wantOK: true,
		},
		{
			name:   "GRC HTTPS with .git suffix",
			raw:    "https://git-codecommit.eu-west-2.amazonaws.com/v1/repos/widgets.git",
			want:   CodeCommitRepo{Region: "eu-west-2", Name: "widgets"},
			wantOK: true,
		},
		{
			name:   "surrounding whitespace tolerated",
			raw:    "  codecommit::ap-south-1://tablespace-api  ",
			want:   CodeCommitRepo{Region: "ap-south-1", Name: "tablespace-api"},
			wantOK: true,
		},
		// Non-CodeCommit URLs must be rejected — this is how callers tell a
		// CodeCommit registration from a plain HTTPS one.
		{name: "github URL", raw: "https://github.com/acme/checkout.git"},
		{name: "bitbucket URL", raw: "https://bitbucket.org/team/repo.git"},
		{name: "self-hosted git URL", raw: "https://git.internal.example.com/v1/repos/x"},
		{name: "empty", raw: ""},
		{name: "prefix but no region", raw: "codecommit::://repo"},
		{name: "prefix but no repo", raw: "codecommit::ap-south-1://"},
		{name: "prefix without separator", raw: "codecommit::ap-south-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseCodeCommitURL(tt.raw)
			if ok != tt.wantOK {
				t.Fatalf("ParseCodeCommitURL(%q) ok = %v, want %v", tt.raw, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("ParseCodeCommitURL(%q) = %+v, want %+v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestCodeCommitCloneURLRoundTrip(t *testing.T) {
	grc := CodeCommitCloneURL("ap-south-1", "tablespace-api")
	if grc != "codecommit::ap-south-1://tablespace-api" {
		t.Errorf("CodeCommitCloneURL = %q", grc)
	}
	https := CodeCommitHTTPSCloneURL("ap-south-1", "tablespace-api")
	if https != "https://git-codecommit.ap-south-1.amazonaws.com/v1/repos/tablespace-api" {
		t.Errorf("CodeCommitHTTPSCloneURL = %q", https)
	}
	// Both forms must parse back to the same repo.
	for _, u := range []string{grc, https} {
		got, ok := ParseCodeCommitURL(u)
		if !ok {
			t.Fatalf("ParseCodeCommitURL(%q) not ok", u)
		}
		want := CodeCommitRepo{Region: "ap-south-1", Name: "tablespace-api"}
		if got != want {
			t.Errorf("ParseCodeCommitURL(%q) = %+v, want %+v", u, got, want)
		}
	}
}

// ccStub serves the CodeCommit JSON-1.1 GetFile operation. handler returns the
// HTTP status and the raw JSON body to reply with.
func ccStub(t *testing.T, handler func(target string, in map[string]any) (int, string)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		code, body := handler(r.Header.Get("X-Amz-Target"), in)
		if code != http.StatusOK {
			// AWS JSON-1.1 signals the fault type in this header.
			var typed struct {
				Type string `json:"__type"`
			}
			_ = json.Unmarshal([]byte(body), &typed)
			w.Header().Set("X-Amzn-Errortype", typed.Type)
		}
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CODECOMMIT_API_BASE", srv.URL)
	return srv
}

// TestFetchCodeCommitGetFile covers the happy path: the request names the
// repository and the exact commit, and the returned blob is the file's bytes.
func TestFetchCodeCommitGetFile(t *testing.T) {
	const wantYAML = "stages: [test]\njobs:\n  t: { stage: test, script: echo hi }\n"
	var gotTarget string
	var gotIn map[string]any
	ccStub(t, func(target string, in map[string]any) (int, string) {
		gotTarget, gotIn = target, in
		body, _ := json.Marshal(map[string]any{
			"blobId":      "b1",
			"commitId":    "9fceb02d",
			"filePath":    ".forge-ci.yml",
			"fileMode":    "NORMAL",
			"fileSize":    len(wantYAML),
			"fileContent": base64.StdEncoding.EncodeToString([]byte(wantYAML)),
		})
		return http.StatusOK, string(body)
	})

	f := NewFetcher()
	content, found, err := f.FetchFile(context.Background(), FetchRequest{
		Provider: "codecommit",
		Repo:     "tablespace-api",
		SHA:      "9fceb02d0ae598e95dc970b74767f19372d61af8",
		Path:     ".forge-ci.yml",
		CloneURL: "codecommit::ap-south-1://tablespace-api",
	})
	if err != nil || !found {
		t.Fatalf("FetchFile: found=%v err=%v", found, err)
	}
	if string(content) != wantYAML {
		t.Errorf("content = %q, want %q", content, wantYAML)
	}
	if !strings.HasSuffix(gotTarget, "GetFile") {
		t.Errorf("X-Amz-Target = %q, want a GetFile target", gotTarget)
	}
	if gotIn["repositoryName"] != "tablespace-api" {
		t.Errorf("repositoryName = %v", gotIn["repositoryName"])
	}
	if gotIn["commitSpecifier"] != "9fceb02d0ae598e95dc970b74767f19372d61af8" {
		t.Errorf("commitSpecifier = %v — config must be read at the event sha", gotIn["commitSpecifier"])
	}
	if gotIn["filePath"] != ".forge-ci.yml" {
		t.Errorf("filePath = %v", gotIn["filePath"])
	}
}

// An unset Path must default to .forge-ci.yml, matching the other providers.
func TestFetchCodeCommitDefaultPath(t *testing.T) {
	var gotPath any
	ccStub(t, func(_ string, in map[string]any) (int, string) {
		gotPath = in["filePath"]
		body, _ := json.Marshal(map[string]any{
			"blobId": "b1", "commitId": "c1", "filePath": ".forge-ci.yml",
			"fileMode": "NORMAL", "fileSize": 2,
			"fileContent": base64.StdEncoding.EncodeToString([]byte("{}")),
		})
		return http.StatusOK, string(body)
	})
	f := NewFetcher()
	if _, found, err := f.FetchFile(context.Background(), FetchRequest{
		Provider: "codecommit", Repo: "r", SHA: "abc",
		CloneURL: "codecommit::ap-south-1://r",
	}); err != nil || !found {
		t.Fatalf("FetchFile: found=%v err=%v", found, err)
	}
	if gotPath != DefaultConfigPath {
		t.Errorf("filePath = %v, want %q", gotPath, DefaultConfigPath)
	}
}

// Every "nothing there" fault must degrade to (found=false, nil) so the caller
// falls back to the registered config — exactly as a GitHub 404 does.
func TestFetchCodeCommitNotFoundFallsBack(t *testing.T) {
	for _, faultType := range []string{
		"FileDoesNotExistException",
		"PathDoesNotExistException",
		"CommitDoesNotExistException",
		"RepositoryDoesNotExistException",
	} {
		t.Run(faultType, func(t *testing.T) {
			ccStub(t, func(string, map[string]any) (int, string) {
				return http.StatusBadRequest,
					`{"__type":"` + faultType + `","message":"not here"}`
			})
			f := NewFetcher()
			content, found, err := f.FetchFile(context.Background(), FetchRequest{
				Provider: "codecommit", Repo: "r", SHA: "abc",
				CloneURL: "codecommit::ap-south-1://r",
			})
			if err != nil {
				t.Fatalf("err = %v, want nil (a missing file is a fall-back, not a failure)", err)
			}
			if found {
				t.Error("found = true, want false")
			}
			if content != nil {
				t.Errorf("content = %q, want nil", content)
			}
		})
	}
}

// A genuine failure must surface as an error carrying the AWS error CODE only —
// never the response body, which can echo request material.
func TestFetchCodeCommitErrorLeaksNoBody(t *testing.T) {
	const secretish = "arn:aws:iam::123456789012:role/super-secret-role"
	ccStub(t, func(string, map[string]any) (int, string) {
		return http.StatusForbidden,
			`{"__type":"AccessDeniedException","message":"not authorized for ` + secretish + `"}`
	})
	f := NewFetcher()
	_, found, err := f.FetchFile(context.Background(), FetchRequest{
		Provider: "codecommit", Repo: "r", SHA: "abc",
		CloneURL: "codecommit::ap-south-1://r",
	})
	if err == nil {
		t.Fatal("expected an error for AccessDenied")
	}
	if found {
		t.Error("found = true, want false")
	}
	if !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("err = %v, want it to name the AWS error code", err)
	}
	if strings.Contains(err.Error(), secretish) {
		t.Errorf("error text leaked the response body: %v", err)
	}
}

// A CodeCommit registration whose clone URL carries no region cannot be called.
// ResolvePipelineConfig logs this and falls back, but the error must say what is
// wrong with the registration.
func TestFetchCodeCommitBadCloneURL(t *testing.T) {
	f := NewFetcher()
	_, found, err := f.FetchFile(context.Background(), FetchRequest{
		Provider: "codecommit", Repo: "r", SHA: "abc", CloneURL: "https://example.com/r.git",
	})
	if err == nil {
		t.Fatal("expected an error for a non-CodeCommit clone URL")
	}
	if found {
		t.Error("found = true, want false")
	}
	if !strings.Contains(err.Error(), "codecommit::") {
		t.Errorf("err = %v, want it to state the expected URL form", err)
	}
}

// Regression: adding the CodeCommit branch must not change how the other
// providers resolve. A single-segment repo name is still invalid for GitHub, and
// provider "other" still reports "fall back" rather than an error.
func TestFetchNonCodeCommitProvidersUnchanged(t *testing.T) {
	f := NewFetcher()

	if _, _, err := f.FetchFile(context.Background(), FetchRequest{
		Provider: "github", Repo: "no-slash", SHA: "abc",
	}); err == nil {
		t.Error("github with a single-segment repo should still be rejected")
	}

	content, found, err := f.FetchFile(context.Background(), FetchRequest{
		Provider: "other", Repo: "acme/thing", SHA: "abc",
		CloneURL: "codecommit::ap-south-1://thing", // ignored: provider is not codecommit
	})
	if err != nil || found || content != nil {
		t.Errorf(`provider "other" = (%q, %v, %v), want (nil, false, nil)`, content, found, err)
	}
}
