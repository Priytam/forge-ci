package kube

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestSourceAuthHeader(t *testing.T) {
	basic := func(u, p string) string { return "Basic " + base64.StdEncoding.EncodeToString([]byte(u+":"+p)) }
	for _, c := range []struct {
		name string
		src  Source
		want string
	}{
		{"no token", Source{URL: "https://github.com/acme/deploy.git"}, ""},
		{"github auto", Source{URL: "https://github.com/acme/deploy.git", Token: "ghs_x"}, basic("x-access-token", "ghs_x")},
		{"github enterprise cloud", Source{URL: "https://acme.ghe.com/acme/deploy.git", Token: "t"}, basic("x-access-token", "t")},
		{"gitlab auto", Source{URL: "https://gitlab.com/acme/deploy.git", Token: "glpat"}, basic("oauth2", "glpat")},
		{"other host auto", Source{URL: "https://git.acme.internal/deploy.git", Token: "t"}, "Bearer t"},
		{"forced bearer on github", Source{URL: "https://github.com/a/b", Token: "t", Auth: "bearer"}, "Bearer t"},
		{"forced basic elsewhere", Source{URL: "https://git.acme.internal/d.git", Token: "t", Auth: "basic"}, basic("git", "t")},
		{"custom username", Source{URL: "https://git.acme.internal/d.git", Token: "t", Auth: "basic", Username: "bot"}, basic("bot", "t")},
	} {
		if got := authHeader(c.src); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// TestSourceFetchSendsAuthHeader: the header reaches the server on git's
// first request (git-over-HTTPS never sees the token in a URL).
func TestSourceFetchSendsAuthHeader(t *testing.T) {
	var mu sync.Mutex
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Header.Get("Authorization")
		mu.Unlock()
		http.Error(w, "no", http.StatusForbidden)
	}))
	defer srv.Close()
	r, err := newRepo(Source{URL: srv.URL + "/acme/deploy.git", Token: "tok", Auth: "basic", Username: "x-access-token", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.fetch(context.Background()); err == nil {
		t.Fatal("fetch from a 403 server succeeded")
	}
	mu.Lock()
	defer mu.Unlock()
	if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:tok")); got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
}
