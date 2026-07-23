# Memorystore for Redis with a private IP on the given VPC, for the Forge
# high-volume log tier (live per-job buffer + pub/sub fan-out). The buffer is
# ephemeral (finished jobs are archived to GCS), so BASIC tier with no failover
# is the sensible default; switch to STANDARD_HA for production resilience.

resource "google_redis_instance" "this" {
  name           = "${var.name}-redis"
  region         = var.region
  tier           = var.tier
  memory_size_gb = var.memory_size_gb

  authorized_network = var.authorized_network
  connect_mode       = "PRIVATE_SERVICE_ACCESS"

  redis_version           = var.redis_version
  transit_encryption_mode = var.transit_encryption_mode

  depends_on = [var.private_vpc_connection]
}
