output "primary_endpoint" {
  description = "Primary endpoint host for the replication group."
  value       = aws_elasticache_replication_group.this.primary_endpoint_address
}

output "redis_url" {
  description = "REDIS_URL wired into the Helm release. rediss:// when transit encryption is on; includes the AUTH token when set."
  value = format(
    "%s://%s%s:6379/0",
    var.transit_encryption_enabled ? "rediss" : "redis",
    var.auth_token != "" ? format(":%s@", var.auth_token) : "",
    aws_elasticache_replication_group.this.primary_endpoint_address,
  )
  sensitive = true
}
