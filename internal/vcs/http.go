package vcs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// getRaw GETs a file body; found=false on 404. headers carry the host's auth.
func (f *Fetcher) getRaw(ctx context.Context, reqURL, accept string, headers map[string]string, what string) ([]byte, bool, error) {
	b, found, _, err := f.getRawCT(ctx, reqURL, accept, headers, what)
	return b, found, err
}

// getRawCT is getRaw plus the response Content-Type.
func (f *Fetcher) getRawCT(ctx context.Context, reqURL, accept string, headers map[string]string, what string) ([]byte, bool, string, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, false, "", err
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	resp, err := f.client.Do(r)
	if err != nil {
		return nil, false, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // configs are small; cap at 1 MiB
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, false, "", nil
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, false, "", fmt.Errorf("vcs: fetch %s failed: HTTP %d", what, resp.StatusCode)
	}
	return body, true, resp.Header.Get("Content-Type"), nil
}

func bearer(token string) map[string]string {
	if token == "" {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + token}
}

// getJSONHeaders GETs JSON with arbitrary auth headers.
func (f *Fetcher) getJSONHeaders(ctx context.Context, reqURL string, headers map[string]string, v any) error {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}
	for k, val := range headers {
		r.Header.Set(k, val)
	}
	r.Header.Set("Accept", "application/json")
	resp, err := f.client.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("vcs: request failed: HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(v)
}
