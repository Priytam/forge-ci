# VPC-native GKE cluster with a separately-managed node pool. The default node
# pool is removed so the node pool below is the single source of truth.

resource "google_container_cluster" "this" {
  name     = var.name
  location = var.location

  network    = var.network_name
  subnetwork = var.subnet_name

  remove_default_node_pool = true
  initial_node_count       = 1

  networking_mode = "VPC_NATIVE"
  ip_allocation_policy {
    cluster_secondary_range_name  = var.pods_range_name
    services_secondary_range_name = var.services_range_name
  }

  release_channel {
    channel = var.release_channel
  }

  # Keep the deletion guard configurable so non-prod can be torn down.
  deletion_protection = var.deletion_protection
}

resource "google_container_node_pool" "this" {
  name     = "${var.name}-np"
  cluster  = google_container_cluster.this.id
  location = var.location

  node_count = var.node_count

  autoscaling {
    min_node_count = var.min_node_count
    max_node_count = var.max_node_count
  }

  management {
    auto_repair  = true
    auto_upgrade = true
  }

  node_config {
    machine_type = var.machine_type
    disk_size_gb = var.disk_size_gb
    oauth_scopes = ["https://www.googleapis.com/auth/cloud-platform"]

    workload_metadata_config {
      mode = "GKE_METADATA"
    }
  }
}

data "google_client_config" "this" {}
