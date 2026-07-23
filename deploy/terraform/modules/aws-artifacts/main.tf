# S3 artifact bucket + a least-privilege IAM user scoped to that one bucket.
# internal/blob talks S3 with static keys, so we hand it an access key pair.
# (For a keyless setup use IRSA instead; static keys keep the chart's S3_* path
# working unchanged.)

resource "aws_s3_bucket" "this" {
  bucket        = var.bucket_name
  force_destroy = var.force_destroy
  tags          = var.tags
}

resource "aws_s3_bucket_public_access_block" "this" {
  bucket                  = aws_s3_bucket.this.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_versioning" "this" {
  bucket = aws_s3_bucket.this.id
  versioning_configuration {
    status = var.versioning ? "Enabled" : "Suspended"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "this" {
  bucket = aws_s3_bucket.this.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "this" {
  count  = var.expire_days > 0 ? 1 : 0
  bucket = aws_s3_bucket.this.id
  rule {
    id     = "expire-old-artifacts"
    status = "Enabled"
    filter {}
    expiration {
      days = var.expire_days
    }
  }
}

resource "aws_iam_user" "this" {
  name = "${var.name}-artifacts"
  tags = var.tags
}

data "aws_iam_policy_document" "this" {
  statement {
    sid       = "ListBucket"
    actions   = ["s3:ListBucket", "s3:GetBucketLocation"]
    resources = [aws_s3_bucket.this.arn]
  }
  statement {
    sid       = "ObjectRW"
    actions   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"]
    resources = ["${aws_s3_bucket.this.arn}/*"]
  }
}

resource "aws_iam_user_policy" "this" {
  name   = "${var.name}-artifacts"
  user   = aws_iam_user.this.name
  policy = data.aws_iam_policy_document.this.json
}

resource "aws_iam_access_key" "this" {
  user = aws_iam_user.this.name
}
