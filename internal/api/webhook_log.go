package api

import (
	"bytes"
	"io"
	"net/http"
	"regexp"
	"strconv"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// logged wraps a webhook handler so every delivery — accepted, duplicate,
// rejected signature, bad payload — lands in the webhook log with the repo
// it came for, the event, Forge's answer and why.

type captureWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (c *captureWriter) WriteHeader(code int) {
	c.status = code
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if c.body.Len() < 400 {
		c.body.Write(b)
	}
	return c.ResponseWriter.Write(b)
}

var (
	errorRe    = regexp.MustCompile(`"error"\s*:\s*"([^"]*)"`)
	pipelineRe = regexp.MustCompile(`"pipeline"\s*:\s*\{\s*"id"\s*:\s*(\d+)`)
)

func (s *Server) logged(provider string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Keep a copy of the body for attribution; the handler reads it again.
		body, _ := io.ReadAll(io.LimitReader(r.Body, int64(s.maxWebhookBytes)+1))
		r.Body = io.NopCloser(bytes.NewReader(body))
		cw := &captureWriter{ResponseWriter: w}
		h(cw, r)
		status := cw.status
		if status == 0 {
			status = http.StatusOK
		}
		e := store.WebhookEntry{
			Provider: provider, Status: status, SourceIP: clientIP(r),
			Repo:       store.RepoFromWebhookBody(provider, body),
			Event:      firstHeader(r, "X-GitHub-Event", "X-Gitlab-Event", "X-Gitea-Event", "X-Forgejo-Event", "X-Event-Key"),
			DeliveryID: firstHeader(r, "X-GitHub-Delivery", "X-Gitlab-Event-UUID", "X-Gitea-Delivery", "X-Forgejo-Delivery", "X-Request-UUID"),
		}
		resp := cw.body.String()
		switch {
		case status == http.StatusOK && bytes.Contains(cw.body.Bytes(), []byte("duplicate")):
			e.Reason = "duplicate"
		case status >= 400:
			if m := errorRe.FindStringSubmatch(resp); m != nil {
				e.Reason = m[1]
			} else {
				e.Reason = http.StatusText(status)
			}
		case status == http.StatusAccepted:
			e.Reason = "ignored"
		case status == http.StatusCreated:
			if m := pipelineRe.FindStringSubmatch(resp); m != nil {
				if id, err := strconv.ParseInt(m[1], 10, 64); err == nil {
					e.PipelineID = &id
				}
			}
		}
		s.store.LogWebhook(r.Context(), e)
	}
}

func firstHeader(r *http.Request, names ...string) string {
	for _, n := range names {
		if v := r.Header.Get(n); v != "" {
			return v
		}
	}
	return ""
}
