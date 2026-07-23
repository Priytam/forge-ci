terraform {
  required_version = ">= 1.6.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.40.0, < 6.0.0"
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

  # Configure a real backend for team use, e.g.:
  # backend "s3" {
  #   bucket         = "my-tf-state"
  #   key            = "forge-ci/aws/terraform.tfstate"
  #   region         = "ap-south-1"
  #   dynamodb_table = "my-tf-locks"
  #   encrypt        = true
  # }
}

provider "aws" {
  region = var.region
}

# The kubernetes + helm providers point at the created EKS cluster, or a
# bring-your-own cluster when create_cluster=false.
provider "kubernetes" {
  host                   = local.cluster_endpoint
  cluster_ca_certificate = base64decode(local.cluster_ca_certificate)
  token                  = local.cluster_token
}

provider "helm" {
  kubernetes {
    host                   = local.cluster_endpoint
    cluster_ca_certificate = base64decode(local.cluster_ca_certificate)
    token                  = local.cluster_token
  }
}
