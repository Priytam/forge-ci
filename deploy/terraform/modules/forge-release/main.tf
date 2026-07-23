# Installs the forge-ci Helm chart and wires cloud-derived secrets in via a
# Kubernetes Secret referenced as secrets.existingSecret. Provider-agnostic:
# the kubernetes + helm providers are configured by the calling root module.

locals {
  secret_name = "${var.release_name}-forge-ci-secrets"

  # Sensitive env delivered to the server through an existingSecret so the
  # cloud-generated DB URL / key / S3 secret never land in Helm values or state
  # as plain chart inputs.
  secret_data = merge(
    {
      DATABASE_URL     = var.database_url
      FORGE_SECRET_KEY = var.forge_secret_key
      RUNNER_TOKEN     = var.runner_token
      WEBHOOK_SECRET   = var.webhook_secret
    },
    var.artifact_store == "s3" && var.s3 != null ? {
      S3_SECRET_KEY = var.s3.secret_key
    } : {},
    # REDIS_URL is delivered through the existingSecret (it may carry a password).
    # Empty when using the bundled in-cluster Redis (redis.enabled derives the
    # URL) or when running without Redis (server falls back to postgres logs).
    var.redis_url != "" ? {
      REDIS_URL = var.redis_url
    } : {}
  )

  base_values = {
    image = {
      registry = var.image_registry
    }
    secrets = {
      existingSecret = kubernetes_secret.forge.metadata[0].name
    }
    postgresql = {
      enabled = var.postgresql_enabled
    }
    # Bundle in-cluster Redis only when no managed Redis URL is supplied.
    redis = {
      enabled = var.redis_enabled
    }
    config = merge(
      {
        runnerAuth  = var.runner_auth
        adminEmails = var.admin_emails
        logBackend  = var.log_backend
        artifacts   = { store = var.artifact_store }
      },
      var.artifact_store == "s3" && var.s3 != null ? {
        artifacts = {
          store = "s3"
          s3 = {
            endpoint  = var.s3.endpoint
            bucket    = var.s3.bucket
            region    = var.s3.region
            useSSL    = tostring(var.s3.use_ssl)
            accessKey = var.s3.access_key
          }
        }
      } : {}
    )
    runner = {
      mode = var.runner_mode
    }
    ingress = {
      enabled     = var.ingress.enabled
      className   = var.ingress.class_name
      host        = var.ingress.host
      annotations = var.ingress.annotations
      tls = {
        enabled    = var.ingress.tls_enabled
        secretName = var.ingress.tls_secret
      }
    }
  }
}

resource "kubernetes_namespace" "forge" {
  count = var.create_namespace ? 1 : 0
  metadata {
    name = var.namespace
    labels = {
      "app.kubernetes.io/managed-by" = "terraform"
      "app.kubernetes.io/part-of"    = "forge-ci"
    }
  }
}

resource "kubernetes_secret" "forge" {
  metadata {
    name      = local.secret_name
    namespace = var.namespace
    labels = {
      "app.kubernetes.io/part-of" = "forge-ci"
    }
  }
  type = "Opaque"
  data = local.secret_data

  depends_on = [kubernetes_namespace.forge]
}

resource "helm_release" "forge" {
  name             = var.release_name
  namespace        = var.namespace
  chart            = var.chart_path
  version          = var.chart_version
  create_namespace = false
  wait             = var.wait
  atomic           = false
  timeout          = 600

  values = [
    yamlencode(local.base_values),
    yamlencode(var.extra_values),
  ]

  depends_on = [
    kubernetes_secret.forge,
    kubernetes_namespace.forge,
  ]
}
