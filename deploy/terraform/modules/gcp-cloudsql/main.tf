# Cloud SQL for Postgres with a private IP on the given VPC. Depends on the
# Private Service Access connection created in the network module.

resource "random_password" "db" {
  count            = var.password == "" ? 1 : 0
  length           = 24
  special          = true
  override_special = "!#$%&*()-_=+"
}

locals {
  password = var.password != "" ? var.password : random_password.db[0].result
}

resource "google_sql_database_instance" "this" {
  name             = "${var.name}-pg"
  region           = var.region
  database_version = var.database_version

  deletion_protection = var.deletion_protection

  settings {
    tier              = var.tier
    availability_type = var.availability_type
    disk_size         = var.disk_size
    disk_autoresize   = true

    backup_configuration {
      enabled                        = true
      point_in_time_recovery_enabled = true
    }

    ip_configuration {
      ipv4_enabled    = false
      private_network = var.private_network_id
    }
  }

  depends_on = [var.private_vpc_connection]
}

resource "google_sql_database" "this" {
  name     = var.db_name
  instance = google_sql_database_instance.this.name
}

resource "google_sql_user" "this" {
  name     = var.username
  instance = google_sql_database_instance.this.name
  password = local.password
}
