variable "aws_region" {
  description = "AWS region for all resources."
  type        = string
  default     = "us-east-1"
}

variable "app_name" {
  description = "Application name — used as a prefix for every resource."
  type        = string
  default     = "conduit"
}

variable "image_tag" {
  description = "Docker image tag to deploy (commit SHA or 'latest')."
  type        = string
  default     = "latest"
}

variable "conduit_desired_count" {
  description = "Number of Conduit ECS tasks to run."
  type        = number
  default     = 2
}

variable "conduit_cpu" {
  description = "Fargate task CPU units (256 = 0.25 vCPU)."
  type        = number
  default     = 256
}

variable "conduit_memory" {
  description = "Fargate task memory in MiB."
  type        = number
  default     = 512
}

variable "etcd_instance_type" {
  description = "EC2 instance type for the etcd node."
  type        = string
  default     = "t3.small"
}

variable "etcd_version" {
  description = "etcd release to install on the EC2 node."
  type        = string
  default     = "v3.5.17"
}

variable "az_count" {
  description = "Number of Availability Zones to use (2 or 3)."
  type        = number
  default     = 2
}
