package autoscale

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// kubernetesProvider scales a Deployment (or StatefulSet) of runners through
// the Kubernetes API's scale subresource — no client library, no kubectl.
//
// Settings: namespace, deployment (required); kind (Deployment, default, or
// StatefulSet); api_url, token, ca (PEM) or insecure=true — all optional:
// unset, the server uses its in-cluster service account
// (/var/run/secrets/kubernetes.io/serviceaccount), which needs
// get/update/patch on deployments/scale in the namespace.
type kubernetesProvider struct{}

func init() { register(kubernetesProvider{}) }

func (kubernetesProvider) Name() string { return "kubernetes" }

func (kubernetesProvider) Validate(s Settings) ([]string, error) {
	if err := requireKeys(s, "namespace", "deployment"); err != nil {
		return nil, err
	}
	switch s.get("kind", "Deployment") {
	case "Deployment", "StatefulSet":
	default:
		return nil, errors.New("kind must be Deployment or StatefulSet")
	}
	if u := s.get("api_url", ""); u != "" && !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return nil, errors.New("api_url must be a URL")
	}
	return []string{"token"}, nil
}

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

type kubeClient struct {
	base, token string
	http        *http.Client
}

func newKubeClient(s Settings) (*kubeClient, error) {
	c := &kubeClient{base: s.get("api_url", ""), token: s.get("token", "")}
	if c.base == "" {
		host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
		if host == "" {
			return nil, errors.New("not running in a cluster: set api_url and token")
		}
		c.base = "https://" + host + ":" + port
	}
	if c.token == "" {
		b, err := os.ReadFile(saDir + "/token")
		if err != nil {
			return nil, errors.New("no token: set token, or run the server in the cluster with a service account")
		}
		c.token = strings.TrimSpace(string(b))
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if s.get("insecure", "") == "true" {
		tlsCfg.InsecureSkipVerify = true
	} else if ca := s.get("ca", ""); ca != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(ca)) {
			return nil, errors.New("ca is not PEM")
		}
		tlsCfg.RootCAs = pool
	} else if b, err := os.ReadFile(saDir + "/ca.crt"); err == nil {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(b)
		tlsCfg.RootCAs = pool
	}
	c.http = &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg}}
	return c, nil
}

func (c *kubeClient) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		var st struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &st)
		return fmt.Errorf("kubernetes %s %s: %s %s", method, path, resp.Status, st.Message)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func scalePath(s Settings) string {
	kind := "deployments"
	if s.get("kind", "Deployment") == "StatefulSet" {
		kind = "statefulsets"
	}
	return fmt.Sprintf("/apis/apps/v1/namespaces/%s/%s/%s/scale", s.get("namespace", ""), kind, s.get("deployment", ""))
}

type scaleObject struct {
	Spec struct {
		Replicas int `json:"replicas"`
	} `json:"spec"`
}

func (kubernetesProvider) Current(ctx context.Context, p Pool) (int, error) {
	c, err := newKubeClient(p.Settings)
	if err != nil {
		return 0, err
	}
	var sc scaleObject
	if err := c.do(ctx, http.MethodGet, scalePath(p.Settings), nil, &sc); err != nil {
		return 0, err
	}
	return sc.Spec.Replicas, nil
}

func (kubernetesProvider) Scale(ctx context.Context, p Pool, desired int) error {
	c, err := newKubeClient(p.Settings)
	if err != nil {
		return err
	}
	patch := map[string]any{"spec": map[string]int{"replicas": desired}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.base+scalePath(p.Settings), bytes.NewReader(mustJSON(patch)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/merge-patch+json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("kubernetes scale: %s %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	return nil
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
