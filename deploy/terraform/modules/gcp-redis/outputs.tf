output "host" {
  value = google_redis_instance.this.host
}

output "port" {
  value = google_redis_instance.this.port
}

output "redis_url" {
  description = "REDIS_URL wired into the Helm release, via the instance private IP. rediss:// when in-transit TLS is enabled."
  value = format(
    "%s://%s:%d/0",
    var.transit_encryption_mode == "SERVER_AUTHENTICATION" ? "rediss" : "redis",
    google_redis_instance.this.host,
    google_redis_instance.this.port,
  )
  sensitive = true
}
