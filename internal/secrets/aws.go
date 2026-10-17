package secrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// awsProvider reads AWS Secrets Manager.
//
//	secrets:
//	  DB_URL:  {aws: "prod/db"}             # whole SecretString
//	  API_KEY: {aws: "prod/api#key"}        # one key of a JSON secret
//
// Identity: AWS_ROLE_ARN (job variable) is assumed with the job's OIDC
// token (STS AssumeRoleWithWebIdentity — the same trust policy as the
// docs/oidc.md AWS recipe); without it, the runner's own credential chain.
// Region: AWS_REGION / AWS_DEFAULT_REGION from the job, else the runner's.
type awsProvider struct{}

func init() { register(awsProvider{}) }

func (awsProvider) Name() string { return "aws" }

// awsConfig builds an SDK config for the job.
func awsConfig(ctx context.Context, env Env) (aws.Config, error) {
	opts := []func(*config.LoadOptions) error{}
	if r := env.get("AWS_REGION"); r != "" {
		opts = append(opts, config.WithRegion(r))
	} else if r := env.get("AWS_DEFAULT_REGION"); r != "" {
		opts = append(opts, config.WithRegion(r))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return aws.Config{}, err
	}
	if role := env.get("AWS_ROLE_ARN"); role != "" && jobToken(env) != "" {
		tok := jobToken(env)
		cfg.Credentials = aws.NewCredentialsCache(stscreds.NewWebIdentityRoleProvider(
			sts.NewFromConfig(cfg), role, staticToken(tok),
			func(o *stscreds.WebIdentityRoleOptions) { o.RoleSessionName = "forge-job" }))
	}
	if cfg.Region == "" {
		return aws.Config{}, errors.New("no AWS region: set AWS_REGION")
	}
	return cfg, nil
}

func (awsProvider) Fetch(ctx context.Context, env Env, spec proto.SecretSpec) (string, error) {
	id, key, _ := strings.Cut(spec.AWS, "#")
	if id == "" {
		return "", errors.New("aws reference must be a secret name or ARN")
	}
	cfg, err := awsConfig(ctx, env)
	if err != nil {
		return "", err
	}
	out, err := secretsmanager.NewFromConfig(cfg).GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(id)})
	if err != nil {
		return "", err
	}
	var value string
	switch {
	case out.SecretString != nil:
		value = *out.SecretString
	case out.SecretBinary != nil:
		value = base64.StdEncoding.EncodeToString(out.SecretBinary)
	}
	if key == "" {
		return value, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(value), &m); err != nil {
		return "", fmt.Errorf("secret %s is not JSON, so #%s cannot be read", id, key)
	}
	v, ok := m[key]
	if !ok {
		return "", fmt.Errorf("key %q not in secret %s", key, id)
	}
	if s, ok := v.(string); ok {
		return s, nil
	}
	raw, _ := json.Marshal(v)
	return string(raw), nil
}

// ecrCredentials returns a docker username/password for an ECR registry.
func ecrCredentials(ctx context.Context, env Env, registry string) (string, string, error) {
	// 123456789012.dkr.ecr.eu-west-1.amazonaws.com → region eu-west-1
	parts := strings.Split(registry, ".")
	if len(parts) >= 6 && parts[1] == "dkr" && parts[2] == "ecr" {
		if env["AWS_REGION"] == "" {
			env = cloneEnv(env, "AWS_REGION", parts[3])
		}
	}
	cfg, err := awsConfig(ctx, env)
	if err != nil {
		return "", "", err
	}
	out, err := ecr.NewFromConfig(cfg).GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return "", "", err
	}
	if len(out.AuthorizationData) == 0 || out.AuthorizationData[0].AuthorizationToken == nil {
		return "", "", errors.New("ECR returned no authorization token")
	}
	raw, err := base64.StdEncoding.DecodeString(*out.AuthorizationData[0].AuthorizationToken)
	if err != nil {
		return "", "", err
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok {
		return "", "", errors.New("unexpected ECR token format")
	}
	return user, pass, nil
}

func cloneEnv(env Env, k, v string) Env {
	out := Env{}
	for key, val := range env {
		out[key] = val
	}
	out[k] = v
	return out
}

// staticToken hands the SDK the job's OIDC token (stscreds.IdentityTokenRetriever).
type staticToken string

func (t staticToken) GetIdentityToken() ([]byte, error) { return []byte(t), nil }
