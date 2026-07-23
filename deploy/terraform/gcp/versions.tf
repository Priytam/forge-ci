terraform {
  required_version = ">= 1.6.0"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = ">= 5.20.0, < 7.0.0"
    }
    random = {
      source  = "hashicorp/random"
      version = ">= 3.6.0, < 4.0.0"
    }
    helm = {
      source  = "hashicorp/helm"
      version = ">= 2.12.0, < 3.0.0"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = ">= 2.25.0, < 3.0.0"
    }
  }

  # backend "gcs" {
  #   bucket = "my-tf-state"
  #   prefix = "forge-ci/gcp"
  # }
}

provider "google" {
  project = var.project_id
  region  = var.region
}

provider "kubernetes" {
  host                   = local.cluster_host
  cluster_ca_certificate = base64decode(local.cluster_ca_certificate)
  token                  = local.cluster_token
}

provider "helm" {
  kubernetes {
    host                   = local.cluster_host
    cluster_ca_certificate = base64decode(local.cluster_ca_certificate)
    token                  = local.cluster_token
  }
}
