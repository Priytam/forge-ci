package autoscale

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// dockerProvider runs runner containers on a Docker host through the Engine
// API (a unix socket or tcp://host:2375; TLS tcp hosts need a proxy or the
// exec provider). Each container is a runner whose RUNNER_ID is its name, so
// the pool can stop idle ones first.
//
// Settings: image (required, a forge-runner image); host (default
// unix:///var/run/docker.sock); server_url (what the runner connects to;
// default this server's EXTERNAL_URL); runner_token; executor (shell |
// docker, default docker); extra env as env.KEY=value; docker_sock=true
// mounts the host's Docker socket into the runner (for the docker executor).
type dockerProvider struct{}

func init() { register(dockerProvider{}) }

func (dockerProvider) Name() string { return "docker" }

func (dockerProvider) Validate(s Settings) ([]string, error) {
	if err := requireKeys(s, "image"); err != nil {
		return nil, err
	}
	h := s.get("host", "unix:///var/run/docker.sock")
	if !strings.HasPrefix(h, "unix://") && !strings.HasPrefix(h, "tcp://") {
		return nil, errors.New("host must be unix:///path or tcp://host:port")
	}
	return []string{"runner_token"}, nil
}

type dockerClient struct {
	http *http.Client
	base string
}

func newDockerClient(s Settings) *dockerClient {
	host := s.get("host", "unix:///var/run/docker.sock")
	tr := &http.Transport{}
	base := "http://docker"
	if strings.HasPrefix(host, "unix://") {
		sock := strings.TrimPrefix(host, "unix://")
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", sock)
		}
	} else {
		base = "http://" + strings.TrimPrefix(host, "tcp://")
	}
	return &dockerClient{http: &http.Client{Timeout: 60 * time.Second, Transport: tr}, base: base}
}

func (c *dockerClient) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(mustJSON(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
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
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		return fmt.Errorf("docker %s %s: %s %s", method, path, resp.Status, e.Message)
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

const poolLabel = "io.forge-ci.pool"

type dockerContainer struct {
	ID    string   `json:"Id"`
	Names []string `json:"Names"`
	State string   `json:"State"`
}

func (c *dockerClient) list(ctx context.Context, pool string) ([]dockerContainer, error) {
	filters := mustJSON(map[string][]string{"label": {poolLabel + "=" + pool}})
	var out []dockerContainer
	err := c.do(ctx, http.MethodGet, "/containers/json?all=true&filters="+url.QueryEscape(string(filters)), nil, &out)
	return out, err
}

func (dockerProvider) Current(ctx context.Context, p Pool) (int, error) {
	cs, err := newDockerClient(p.Settings).list(ctx, p.Name)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range cs {
		if c.State == "running" || c.State == "created" || c.State == "restarting" {
			n++
		}
	}
	return n, nil
}

func (dockerProvider) Scale(ctx context.Context, p Pool, desired int) error {
	cli := newDockerClient(p.Settings)
	cs, err := cli.list(ctx, p.Name)
	if err != nil {
		return err
	}
	var live []dockerContainer
	for _, c := range cs {
		if c.State == "running" || c.State == "created" || c.State == "restarting" {
			live = append(live, c)
		} else {
			// Exited runners (drained earlier): clean them up.
			_ = cli.do(ctx, http.MethodDelete, "/containers/"+c.ID+"?force=true", nil, nil)
		}
	}
	for i := len(live); i < desired; i++ {
		if err := startRunner(ctx, cli, p, i); err != nil {
			return err
		}
	}
	if len(live) > desired {
		busy := map[string]bool{}
		for _, id := range p.BusyRunnerIDs {
			busy[id] = true
		}
		// Idle first, then the newest busy ones (they drain and requeue).
		sort.SliceStable(live, func(i, j int) bool {
			bi, bj := busy[containerName(live[i])], busy[containerName(live[j])]
			if bi != bj {
				return !bi
			}
			return containerName(live[i]) > containerName(live[j])
		})
		for _, c := range live[:len(live)-desired] {
			// SIGTERM → the runner drains; Docker kills after t seconds.
			if err := cli.do(ctx, http.MethodPost, "/containers/"+c.ID+"/stop?t=120", nil, nil); err != nil {
				return err
			}
			_ = cli.do(ctx, http.MethodDelete, "/containers/"+c.ID, nil, nil)
		}
	}
	return nil
}

func containerName(c dockerContainer) string {
	if len(c.Names) == 0 {
		return c.ID
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// startRunner creates and starts one runner container. The name doubles as
// the runner id and must be unique: pool name + a time-based suffix.
func startRunner(ctx context.Context, cli *dockerClient, p Pool, i int) error {
	name := fmt.Sprintf("forge-%s-%d-%d", p.Name, time.Now().Unix()%100000, i)
	env := []string{
		"RUNNER_ID=" + name,
		"EXECUTOR=" + p.Settings.get("executor", "docker"),
		"RUNNER_TAGS=" + strings.Join(p.Tags, ","),
		"RUNNER_POOL=" + p.Name,
	}
	if u := p.Settings.get("server_url", ""); u != "" {
		env = append(env, "SERVER_URL="+u)
	}
	if t := p.Settings.get("runner_token", ""); t != "" {
		env = append(env, "RUNNER_TOKEN="+t)
	}
	for k, v := range p.Settings {
		if strings.HasPrefix(k, "env.") {
			env = append(env, strings.TrimPrefix(k, "env.")+"="+v)
		}
	}
	hostCfg := map[string]any{"RestartPolicy": map[string]any{"Name": "no"}}
	if p.Settings.get("docker_sock", "") == "true" {
		hostCfg["Binds"] = []string{"/var/run/docker.sock:/var/run/docker.sock"}
	}
	body := map[string]any{
		"Image":      p.Settings.get("image", ""),
		"Env":        env,
		"Labels":     map[string]string{poolLabel: p.Name},
		"HostConfig": hostCfg,
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := cli.do(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(name), body, &created); err != nil {
		return err
	}
	return cli.do(ctx, http.MethodPost, "/containers/"+created.ID+"/start", nil, nil)
}
