import { LinearFlow } from "../DiagramKit";
import type { Recipe } from "../types";

export const deployAwsEc2: Recipe = {
  slug: "deploy-aws-ec2",
  title: "Deploy to AWS EC2 with an approval gate",
  category: "Deploy & release",
  summary:
    "Build, test, then ship to production via CodeDeploy behind a protected environment approval, using masked AWS creds.",
  features: ["environment", "rules", "needs", "artifacts", "protected variables"],
  diagram: () => (
    <LinearFlow
      caption="Build → test → approved deploy"
      steps={[
        { label: "build", tone: "build" },
        { label: "test", tone: "test" },
        { label: "deploy", sub: "production · approval", tone: "gate" },
      ]}
    />
  ),
  scenario: () => (
    <>
      <p>
        A build artifact should reach production only after a human signs off.
        Forge's protected <code>environment</code> gate turns any deploy job into
        an approval checkpoint without extra tooling.
      </p>
      <p>
        The <code>deploy-ec2</code> job sets <code>environment: production</code>.
        When it targets a protected environment, the scheduler holds it in a{" "}
        <strong>blocked</strong> state until an authorized approver releases it —
        enforced by the repo's approval rule (required approvals, allowed roles,
        self-approval policy). Its <code>rules</code> also confine deploys to{" "}
        <code>main</code>.
      </p>
      <p>
        AWS credentials (<code>AWS_ACCESS_KEY_ID</code>,{" "}
        <code>AWS_SECRET_ACCESS_KEY</code>) live as <strong>masked + protected</strong>{" "}
        repo variables so they are redacted in logs and only exposed to protected
        refs. The script syncs the built bundle to S3 and triggers a CodeDeploy
        deployment. (For a no-static-keys variant, see the OIDC recipe.)
      </p>
    </>
  ),
  yaml: `stages: [build, test, deploy]

jobs:
  build:
    stage: build
    image: node:20
    script:
      - npm ci
      - npm run build
    artifacts:
      paths: [dist/]
      expire_in: 1h

  test:
    stage: test
    image: node:20
    needs: [build]
    script:
      - npm ci
      - npm test

  deploy-ec2:
    stage: deploy
    image: amazon/aws-cli:2
    needs: [build, test]
    environment: production
    variables:
      AWS_DEFAULT_REGION: ap-south-1
    rules:
      - if: '$CI_COMMIT_BRANCH == "main"'
        when: on_success
      - when: never
    script:
      - aws s3 cp dist/ "s3://$DEPLOY_BUCKET/" --recursive
      - >
        aws deploy create-deployment
        --application-name acme-web
        --deployment-group-name production
        --s3-location bucket=$DEPLOY_BUCKET,key=releases/$CI_COMMIT_REF_NAME.zip,bundleType=zip
`,
};
