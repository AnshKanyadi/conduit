output "cloudfront_url" {
  description = "HTTPS URL for the Conduit frontend (CloudFront)."
  value       = "https://${aws_cloudfront_distribution.main.domain_name}"
}

output "alb_dns" {
  description = "ALB DNS name — use for WebSocket connections if bypassing CloudFront."
  value       = aws_lb.main.dns_name
}

output "ecr_server_url" {
  description = "ECR repository URL for the Conduit server image."
  value       = aws_ecr_repository.server.repository_url
}

output "ecr_web_url" {
  description = "ECR repository URL for the Conduit frontend image."
  value       = aws_ecr_repository.web.repository_url
}

output "etcd_private_ip" {
  description = "Private IP of the etcd EC2 instance (VPC-internal only)."
  value       = aws_instance.etcd.private_ip
}

output "ecs_cluster_name" {
  description = "ECS cluster name — use with 'aws ecs' CLI commands."
  value       = aws_ecs_cluster.main.name
}

output "s3_frontend_bucket" {
  description = "S3 bucket name — sync 'web/dist/' here after building the frontend."
  value       = aws_s3_bucket.frontend.bucket
}

output "deploy_commands" {
  description = "Quick-reference commands for a manual deploy."
  value       = <<-EOT
    # 1. Authenticate to ECR
    aws ecr get-login-password --region ${var.aws_region} \
      | docker login --username AWS --password-stdin ${aws_ecr_repository.server.repository_url}

    # 2. Push the server image
    docker build -t ${aws_ecr_repository.server.repository_url}:latest .
    docker push ${aws_ecr_repository.server.repository_url}:latest

    # 3. Sync the frontend to S3 (content-hashed assets get max-age=1yr)
    cd web && npm run build
    aws s3 sync dist/ s3://${aws_s3_bucket.frontend.bucket}/ \
      --cache-control "public,max-age=31536000,immutable" \
      --exclude "index.html"
    aws s3 cp dist/index.html s3://${aws_s3_bucket.frontend.bucket}/index.html \
      --cache-control "public,max-age=0,must-revalidate"

    # 4. Force a CloudFront invalidation for index.html
    aws cloudfront create-invalidation \
      --distribution-id ${aws_cloudfront_distribution.main.id} \
      --paths "/index.html"

    # 5. Force a new ECS deployment to pick up the updated image
    aws ecs update-service \
      --cluster ${aws_ecs_cluster.main.name} \
      --service ${aws_ecs_service.server.name} \
      --force-new-deployment
  EOT
}
