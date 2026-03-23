# ---- S3 bucket (frontend assets) ------------------------------------------

resource "aws_s3_bucket" "frontend" {
  bucket = "${var.app_name}-frontend-${data.aws_caller_identity.current.account_id}"
}

data "aws_caller_identity" "current" {}

resource "aws_s3_bucket_versioning" "frontend" {
  bucket = aws_s3_bucket.frontend.id
  versioning_configuration { status = "Enabled" }
}

# Block all public access — CloudFront uses OAC to read the bucket privately.
resource "aws_s3_bucket_public_access_block" "frontend" {
  bucket                  = aws_s3_bucket.frontend.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# ---- CloudFront OAC --------------------------------------------------------
# OAC (Origin Access Control) is the modern replacement for OAI. It uses
# SigV4 signing so the bucket never needs a public policy.

resource "aws_cloudfront_origin_access_control" "frontend" {
  name                              = "${var.app_name}-frontend-oac"
  description                       = "OAC for ${var.app_name} frontend"
  origin_access_control_origin_type = "s3"
  signing_behavior                  = "always"
  signing_protocol                  = "sigv4"
}

# Allow CloudFront to read the bucket via OAC.
resource "aws_s3_bucket_policy" "frontend" {
  bucket = aws_s3_bucket.frontend.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid    = "CloudFrontOACRead"
      Effect = "Allow"
      Principal = {
        Service = "cloudfront.amazonaws.com"
      }
      Action   = "s3:GetObject"
      Resource = "${aws_s3_bucket.frontend.arn}/*"
      Condition = {
        StringEquals = {
          "AWS:SourceArn" = aws_cloudfront_distribution.main.arn
        }
      }
    }]
  })
}

# ---- CloudFront distribution -----------------------------------------------

resource "aws_cloudfront_distribution" "main" {
  enabled             = true
  is_ipv6_enabled     = true
  default_root_object = "index.html"
  price_class         = "PriceClass_100" # US + EU only — cheapest option

  # Origin 1: S3 (static React assets).
  origin {
    domain_name              = aws_s3_bucket.frontend.bucket_regional_domain_name
    origin_id                = "s3-frontend"
    origin_access_control_id = aws_cloudfront_origin_access_control.frontend.id
  }

  # Origin 2: ALB (WebSocket API).
  origin {
    domain_name = aws_lb.main.dns_name
    origin_id   = "alb-server"

    custom_origin_config {
      http_port              = 80
      https_port             = 443
      origin_protocol_policy = "http-only"
      origin_ssl_protocols   = ["TLSv1.2"]
      # Long timeout for WebSocket keep-alive reads.
      origin_read_timeout      = 60
      origin_keepalive_timeout = 60
    }
  }

  # /ws → ALB (WebSocket — must not be cached).
  ordered_cache_behavior {
    path_pattern           = "/ws"
    target_origin_id       = "alb-server"
    viewer_protocol_policy = "allow-all"
    allowed_methods        = ["GET", "HEAD"]
    cached_methods         = ["GET", "HEAD"]

    forwarded_values {
      query_string = false
      headers      = ["Upgrade", "Connection", "Sec-WebSocket-Key",
                      "Sec-WebSocket-Protocol", "Sec-WebSocket-Version"]
      cookies { forward = "none" }
    }

    # Never cache WebSocket responses.
    min_ttl     = 0
    default_ttl = 0
    max_ttl     = 0
  }

  # /metrics → ALB.
  ordered_cache_behavior {
    path_pattern           = "/metrics"
    target_origin_id       = "alb-server"
    viewer_protocol_policy = "allow-all"
    allowed_methods        = ["GET", "HEAD"]
    cached_methods         = ["GET", "HEAD"]

    forwarded_values {
      query_string = false
      cookies { forward = "none" }
    }

    min_ttl     = 0
    default_ttl = 0
    max_ttl     = 0
  }

  # /* → S3 (static assets — cache aggressively via content-hashed filenames).
  default_cache_behavior {
    target_origin_id       = "s3-frontend"
    viewer_protocol_policy = "redirect-to-https"
    allowed_methods        = ["GET", "HEAD"]
    cached_methods         = ["GET", "HEAD"]

    forwarded_values {
      query_string = false
      cookies { forward = "none" }
    }

    min_ttl     = 0
    default_ttl = 86400   # 1 day for un-versioned files (e.g. index.html)
    max_ttl     = 31536000 # 1 year for content-hashed assets
    compress    = true
  }

  # Return index.html for any 403/404 so React's client-side router handles it.
  custom_error_response {
    error_code            = 403
    response_code         = 200
    response_page_path    = "/index.html"
  }

  custom_error_response {
    error_code            = 404
    response_code         = 200
    response_page_path    = "/index.html"
  }

  restrictions {
    geo_restriction { restriction_type = "none" }
  }

  viewer_certificate {
    cloudfront_default_certificate = true
    # To use a custom domain + ACM cert, replace with:
    # acm_certificate_arn      = "arn:aws:acm:us-east-1:ACCOUNT:certificate/UUID"
    # ssl_support_method       = "sni-only"
    # minimum_protocol_version = "TLSv1.2_2021"
  }
}
