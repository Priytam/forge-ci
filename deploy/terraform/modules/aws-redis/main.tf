# Managed Redis (ElastiCache) for the Forge high-volume log tier — the live
# per-job log buffer + pub/sub fan-out. Placed in private subnets with a security
# group that only admits the cluster nodes' VPC CIDR. The buffer is ephemeral
# (finished jobs are archived to object storage), so a single-node group with no
# durable persistence is the sensible default.

resource "aws_elasticache_subnet_group" "this" {
  name       = "${var.name}-redis"
  subnet_ids = var.subnet_ids
  tags       = var.tags
}

resource "aws_security_group" "this" {
  name        = "${var.name}-redis"
  description = "Forge CI Redis (log tier) access"
  vpc_id      = var.vpc_id
  tags        = var.tags

  ingress {
    description = "Redis from within the VPC"
    from_port   = 6379
    to_port     = 6379
    protocol    = "tcp"
    cidr_blocks = var.allowed_cidr_blocks
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_elasticache_replication_group" "this" {
  replication_group_id = "${var.name}-redis"
  description          = "Forge CI log-tier Redis"

  engine         = "redis"
  engine_version = var.engine_version
  node_type      = var.node_type
  port           = 6379

  num_cache_clusters         = var.num_cache_clusters
  automatic_failover_enabled = var.num_cache_clusters > 1
  multi_az_enabled           = var.num_cache_clusters > 1

  subnet_group_name  = aws_elasticache_subnet_group.this.name
  security_group_ids = [aws_security_group.this.id]

  # The log buffer is transient; keep snapshots off by default.
  snapshot_retention_limit = var.snapshot_retention_limit

  # In-transit / at-rest encryption. auth_token requires transit encryption.
  transit_encryption_enabled = var.transit_encryption_enabled
  at_rest_encryption_enabled = var.at_rest_encryption_enabled
  auth_token                 = var.auth_token != "" ? var.auth_token : null

  apply_immediately = true
  tags              = var.tags
}
