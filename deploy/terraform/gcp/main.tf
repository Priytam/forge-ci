# Root env: GKE + Cloud SQL Postgres + GCS artifacts (S3-interop) + Secret
# Manager + the Forge CI Helm release. Toggle create_cluster / create_vpc to
# install into an existing cluster/VPC instead.

locals {
  gke_location       = var.gke_location != "" ? var.gke_location : var.region
  artifacts_location = var.artifacts_location != "" ? var.artifacts_location : var.region

  # Network selection.
  network_id             = var.create_vpc ? module.network[0].network_id : var.byo_network_id
  network_name           = var.create_vpc ? module.network[0].network_name : var.byo_network_name
  subnet_name            = var.create_vpc ? module.network[0].subnet_name : var.byo_subnet_name
  pods_range_name        = var.create_vpc ? module.network[0].pods_range_name : var.byo_pods_range_name
  services_range_name    = var.create_vpc ? module.network[0].services_range_name : var.byo_services_range_name
  private_vpc_connection = var.create_vpc ? module.network[0].private_vpc_connection : var.byo_private_vpc_connection

  # Cluster (kubernetes/helm provider) selection. GKE endpoint has no scheme.
  cluster_host           = var.create_cluster ? "https://${module.gke[0].cluster_endpoint}" : var.byo_cluster_endpoint
  cluster_ca_certificate = var.create_cluster ? module.gke[0].cluster_ca_certificate : var.byo_cluster_ca_certificate
  cluster_token          = var.create_cluster ? module.gke[0].cluster_token : var.byo_cluster_token
}

module "network" {
  count         = var.create_vpc ? 1 : 0
  source        = "../modules/gcp-network"
  name          = var.name
  region        = var.region
  subnet_cidr   = var.subnet_cidr
  pods_cidr     = var.pods_cidr
  services_cidr = var.services_cidr
}

module "gke" {
  count    = var.create_cluster ? 1 : 0
  source   = "../modules/gcp-gke"
  name     = var.name
  location = local.gke_location

  network_name        = local.network_name
  subnet_name         = local.subnet_name
  pods_range_name     = local.pods_range_name
  services_range_name = local.services_range_name

  machine_type        = var.machine_type
  node_count          = var.node_count
  min_node_count      = var.min_node_count
  max_node_count      = var.max_node_count
  deletion_protection = var.cluster_deletion_protection
}

module "database" {
  source = "../modules/gcp-cloudsql"
  name   = var.name
  region = var.region

  private_network_id     = local.network_id
  private_vpc_connection = local.private_vpc_connection

  tier                = var.db_tier
  availability_type   = var.db_availability_type
  deletion_protection = var.db_deletion_protection
}

module "artifacts" {
  source        = "../modules/gcp-artifacts"
  bucket_name   = var.artifacts_bucket_name
  location      = local.artifacts_location
  expire_days   = var.artifacts_expire_days
  force_destroy = var.artifacts_force_destroy
}

module "secret" {
  source                = "../modules/gcp-secret"
  secret_id             = "${var.name}-app"
  generate_runner_token = var.runner_auth == "on"
  webhook_secret        = var.webhook_secret
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

  depends_on = [module.gke]
}
