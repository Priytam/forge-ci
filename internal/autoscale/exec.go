package autoscale

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// execProvider delegates to commands, for everything else: cloud VM groups
// (aws autoscaling set-desired-capacity, gcloud compute instance-groups
// managed resize, az vmss scale), Terraform, Nomad, a bare-metal provisioner.
//
// Settings: count_cmd prints the current runner count on stdout; scale_cmd
// sets it. Both run with sh -c and the environment FORGE_POOL, FORGE_TAGS,
// FORGE_DESIRED (scale only), FORGE_CURRENT, FORGE_MIN, FORGE_MAX, plus
// env.KEY=value settings. Commands run on the server host, so the server
// image needs the CLI in question (mount it, or wrap it in a sidecar that
// the command calls).
type execProvider struct{}

func init() { register(execProvider{}) }

func (execProvider) Name() string { return "exec" }

func (execProvider) Validate(s Settings) ([]string, error) {
	if err := requireKeys(s, "count_cmd", "scale_cmd"); err != nil {
		return nil, err
	}
	var secrets []string
	for k := range s {
		lk := strings.ToLower(k)
		if strings.HasPrefix(k, "env.") && (strings.Contains(lk, "token") || strings.Contains(lk, "secret") || strings.Contains(lk, "key") || strings.Contains(lk, "password")) {
			secrets = append(secrets, k)
		}
	}
	return secrets, nil
}

func execEnv(p Pool, current, desired int) []string {
	env := append(os.Environ(),
		"FORGE_POOL="+p.Name,
		"FORGE_TAGS="+strings.Join(p.Tags, ","),
		"FORGE_CURRENT="+strconv.Itoa(current),
		"FORGE_MIN="+strconv.Itoa(p.Min),
		"FORGE_MAX="+strconv.Itoa(p.Max),
		"FORGE_BUSY_RUNNERS="+strings.Join(p.BusyRunnerIDs, ","),
	)
	if desired >= 0 {
		env = append(env, "FORGE_DESIRED="+strconv.Itoa(desired))
	}
	for k, v := range p.Settings {
		if strings.HasPrefix(k, "env.") {
			env = append(env, strings.TrimPrefix(k, "env.")+"="+v)
		}
	}
	return env
}

func runCmd(ctx context.Context, command string, env []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		return "", fmt.Errorf("%v: %s", err, msg)
	}
	return strings.TrimSpace(out.String()), nil
}

func (execProvider) Current(ctx context.Context, p Pool) (int, error) {
	out, err := runCmd(ctx, p.Settings["count_cmd"], execEnv(p, -1, -1))
	if err != nil {
		return 0, fmt.Errorf("count_cmd: %w", err)
	}
	lines := strings.Fields(out)
	if len(lines) == 0 {
		return 0, errors.New("count_cmd printed nothing (expected a number)")
	}
	n, err := strconv.Atoi(lines[len(lines)-1])
	if err != nil || n < 0 {
		return 0, fmt.Errorf("count_cmd printed %q, expected a number", out)
	}
	return n, nil
}

func (execProvider) Scale(ctx context.Context, p Pool, desired int) error {
	current, _ := execProvider{}.Current(ctx, p)
	if _, err := runCmd(ctx, p.Settings["scale_cmd"], execEnv(p, current, desired)); err != nil {
		return fmt.Errorf("scale_cmd: %w", err)
	}
	return nil
}
