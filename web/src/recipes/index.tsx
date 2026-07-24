import type { Recipe } from "./types";
import { nodejsApp } from "./entries/nodejs-app";
import { goService } from "./entries/go-service";
import { javaGradle } from "./entries/java-gradle";
import { dockerBuildScanPush } from "./entries/docker-build-scan-push";
import { deployAwsEc2 } from "./entries/deploy-aws-ec2";
import { gitopsKubernetes } from "./entries/gitops-kubernetes";
import { terraformIac } from "./entries/terraform-iac";
import { monorepo } from "./entries/monorepo";
import { securityFirst } from "./entries/security-first";
import { postgresIntegration } from "./entries/postgres-integration";
import { oidcAwsKeyless } from "./entries/oidc-aws-keyless";
import { nightlyAndPr } from "./entries/nightly-and-pr";

export type { Recipe } from "./types";
export { FeatureChips } from "./types";

/**
 * The recipe library. Append new entries here (and add the module under
 * entries/) to grow the gallery — categories and the index/sidebar derive
 * automatically from this array, so nothing else needs touching.
 */
export const RECIPES: Recipe[] = [
  nodejsApp,
  goService,
  javaGradle,
  dockerBuildScanPush,
  deployAwsEc2,
  gitopsKubernetes,
  terraformIac,
  monorepo,
  securityFirst,
  postgresIntegration,
  oidcAwsKeyless,
  nightlyAndPr,
];

/** Distinct categories in first-seen order. */
export const RECIPE_CATEGORIES: string[] = RECIPES.reduce<string[]>(
  (acc, r) => (acc.includes(r.category) ? acc : [...acc, r.category]),
  []
);

export function findRecipe(slug: string | undefined): Recipe | undefined {
  return RECIPES.find((r) => r.slug === slug);
}
