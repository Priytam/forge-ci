# Root env: EKS + RDS Postgres + S3 artifacts + Secrets Manager + the Forge CI
# Helm release. Every major piece is a toggle so this can either build a full
# stack or install into a bring-your-own cluster/VPC.

locals {
  # Network selection.
  vpc_id             = var.create_vpc ? module.network[0].vpc_id : var.byo_vpc_id
  vpc_cidr           = var.create_vpc ? var.vpc_cidr : var.byo_vpc_cidr
  private_subnet_ids = var.create_vpc ? module.network[0].private_subnet_ids : var.byo_private_subnet_ids
  public_subnet_ids  = var.create_vpc ? module.network[0].public_subnet_ids : var.byo_public_subnet_ids

  # Cluster (kubernetes/helm provider) selection.
  cluster_endpoint       = var.create_cluster ? module.eks[0].cluster_endpoint : var.byo_cluster_endpoint
  cluster_ca_certificate = var.create_cluster ? module.eks[0].cluster_ca_certificate : var.byo_cluster_ca_certificate
  cluster_token          = var.create_cluster ? module.eks[0].cluster_token : var.byo_cluster_token
}

module "network" {
  count    = var.create_vpc ? 1 : 0
  source   = "../modules/aws-network"
  name     = var.name
  vpc_cidr = var.vpc_cidr
  az_count = var.az_count
  tags     = var.tags
}

module "eks" {
  count              = var.create_cluster ? 1 : 0
  source             = "../modules/aws-eks"
  name               = var.name
  kubernetes_version = var.kubernetes_version

  private_subnet_ids  = local.private_subnet_ids
  public_subnet_ids   = local.public_subnet_ids
  node_instance_types = var.node_instance_types
  node_desired_size   = var.node_desired_size
  node_min_size       = var.node_min_size
  node_max_size       = var.node_max_size
  tags                = var.tags
}

module "database" {
  source = "../modules/aws-rds"
  name   = var.name

  vpc_id              = local.vpc_id
  subnet_ids          = local.private_subnet_ids
  allowed_cidr_blocks = [local.vpc_cidr]

  instance_class      = var.db_instance_class
  multi_az            = var.db_multi_az
  deletion_protection = var.db_deletion_protection
  skip_final_snapshot = var.db_skip_final_snapshot
  tags                = var.tags
}

module "artifacts" {
  source        = "../modules/aws-artifacts"
  name          = var.name
  bucket_name   = var.artifacts_bucket_name
  region        = var.region
  expire_days   = var.artifacts_expire_days
  force_destroy = var.artifacts_force_destroy
  tags          = var.tags
}

module "secret" {
  source                = "../modules/aws-secret"
  secret_name           = "${var.name}/app"
  generate_runner_token = var.runner_auth == "on"
  webhook_secret        = var.webhook_secret
  tags                  = var.tags
}

module "forge" {
  source = "../modules/forge-release"

  release_name     = "forge"
  namespace        = var.namespace
  create_namespace = true
  chart_path       = var.chart_path

  database_url     = module.database.database_url
  forge_secret_key = module.secret.forge_secret_key
  runner_token     = module.secret.runner_token
  webhook_secret   = var.webhook_secret

  artifact_store = "s3"
  s3 = {
    endpoint   = module.artifacts.s3_endpoint
    bucket     = module.artifacts.s3_bucket
    region     = module.artifacts.s3_region
    use_ssl    = true
    access_key = module.artifacts.s3_access_key
    secret_key = module.artifacts.s3_secret_key
  }

  image_registry     = var.image_registry
  runner_mode        = var.runner_mode
  runner_auth        = var.runner_auth
  admin_emails       = var.admin_emails
  postgresql_enabled = false

  ingress = {
    enabled     = var.ingress_enabled
    class_name  = var.ingress_class_name
    host        = var.domain
    tls_enabled = var.ingress_tls_enabled
    tls_secret  = "forge-ci-tls"
    annotations = var.ingress_annotations
  }

  # EKS must exist (and its provider be usable) before the release installs.
  depends_on = [module.eks]
}
