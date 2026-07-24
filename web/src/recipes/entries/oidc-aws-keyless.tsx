import { LinearFlow } from "../DiagramKit";
import type { Recipe } from "../types";

export const oidcAwsKeyless: Recipe = {
  slug: "oidc-aws-keyless",
  title: "Keyless AWS deploy via OIDC",
  category: "Security & compliance",
  summary:
    "Exchange the pipeline's short-lived OIDC token for temporary AWS credentials — no static access keys stored anywhere.",
  features: ["FORGE_OIDC_TOKEN", "environment", "rules", "CI_PIPELINE_SOURCE"],
  diagram: () => (
    <LinearFlow
      caption="OIDC token → STS creds → deploy"
      steps={[
        { label: "OIDC token", sub: "$FORGE_OIDC_TOKEN", tone: "commit" },
        { label: "assume-role", sub: "AWS STS", tone: "gate" },
        { label: "deploy", sub: "temp creds", tone: "deploy" },
      ]}
    />
  ),
  scenario: () => (
    <>
      <p>
        Long-lived cloud keys stored in CI are the most common credential-leak
        vector. With OIDC federation there are <strong>no static keys</strong> —
        the pipeline proves its identity with a short-lived token and AWS hands
        back temporary credentials.
      </p>
      <p>
        Forge injects <code>$FORGE_OIDC_TOKEN</code> into the job. The script
        passes it to <code>aws sts assume-role-with-web-identity</code> against a
        pre-configured IAM role (trust policy pinned to Forge's OIDC issuer), then
        exports the returned <code>AccessKeyId</code> / <code>SecretAccessKey</code>{" "}
        / <code>SessionToken</code> for the rest of the job. Those credentials
        expire automatically with the session.
      </p>
      <p>
        The deploy still runs behind a protected <code>production</code>{" "}
        environment and is gated to <code>main</code> by <code>rules</code>. See
        the OIDC guide in the docs for the IAM trust-policy setup.
      </p>
    </>
  ),
  yaml: `stages: [deploy]

jobs:
  deploy-oidc:
    stage: deploy
    image: amazon/aws-cli:2
    environment: production
    variables:
      AWS_DEFAULT_REGION: ap-south-1
      AWS_ROLE_ARN: arn:aws:iam::123456789012:role/forge-deploy
    rules:
      - if: '$CI_COMMIT_BRANCH == "main"'
        when: on_success
      - when: never
    script:
      - >
        CREDS=$(aws sts assume-role-with-web-identity
        --role-arn "$AWS_ROLE_ARN"
        --role-session-name "forge-$CI_PIPELINE_SOURCE"
        --web-identity-token "$FORGE_OIDC_TOKEN"
        --query Credentials --output json)
      - export AWS_ACCESS_KEY_ID=$(echo "$CREDS" | jq -r .AccessKeyId)
      - export AWS_SECRET_ACCESS_KEY=$(echo "$CREDS" | jq -r .SecretAccessKey)
      - export AWS_SESSION_TOKEN=$(echo "$CREDS" | jq -r .SessionToken)
      - aws sts get-caller-identity
      - aws s3 sync dist/ "s3://$DEPLOY_BUCKET/"
`,
};
