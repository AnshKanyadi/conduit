# ---- ECS Cluster -----------------------------------------------------------

resource "aws_ecs_cluster" "main" {
  name = var.app_name

  setting {
    name  = "containerInsights"
    value = "enabled"
  }
}

resource "aws_ecs_cluster_capacity_providers" "main" {
  cluster_name       = aws_ecs_cluster.main.name
  capacity_providers = ["FARGATE", "FARGATE_SPOT"]

  default_capacity_provider_strategy {
    capacity_provider = "FARGATE"
    weight            = 1
  }
}

# ---- IAM roles -------------------------------------------------------------

# Task execution role — used by the ECS agent to pull images and write logs.
resource "aws_iam_role" "ecs_execution" {
  name = "${var.app_name}-ecs-execution"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ecs-tasks.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy_attachment" "ecs_execution" {
  role       = aws_iam_role.ecs_execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

# Task role — the permissions the Conduit container itself gets.
# Currently empty (Conduit contacts etcd directly; no AWS API calls needed).
resource "aws_iam_role" "ecs_task" {
  name = "${var.app_name}-ecs-task"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ecs-tasks.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

# ---- CloudWatch log group --------------------------------------------------

resource "aws_cloudwatch_log_group" "server" {
  name              = "/ecs/${var.app_name}"
  retention_in_days = 30
}

# ---- Task definition -------------------------------------------------------

resource "aws_ecs_task_definition" "server" {
  family                   = var.app_name
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.conduit_cpu
  memory                   = var.conduit_memory
  execution_role_arn       = aws_iam_role.ecs_execution.arn
  task_role_arn            = aws_iam_role.ecs_task.arn

  container_definitions = jsonencode([{
    name      = var.app_name
    image     = "${aws_ecr_repository.server.repository_url}:${var.image_tag}"
    essential = true

    portMappings = [{
      containerPort = 8080
      protocol      = "tcp"
    }]

    environment = [
      # etcd endpoint — private IP injected here.
      { name = "CONDUIT_ETCD_ENDPOINTS", value = "${aws_instance.etcd.private_ip}:2379" },
      # NODE_ADDR is the ECS task private IP; ECS injects this via the
      # metadata endpoint at runtime. A sidecar or init-container can read
      # the task metadata and populate CONDUIT_NODE_ADDR. For now we leave it
      # unset so the server defaults to ":8080" (sufficient with ALB stickiness).
    ]

    logConfiguration = {
      logDriver = "awslogs"
      options = {
        "awslogs-group"         = aws_cloudwatch_log_group.server.name
        "awslogs-region"        = var.aws_region
        "awslogs-stream-prefix" = "server"
      }
    }

    # Health check via the /metrics Prometheus endpoint.
    healthCheck = {
      command     = ["CMD-SHELL", "wget -qO- http://localhost:8080/metrics || exit 1"]
      interval    = 30
      timeout     = 5
      retries     = 3
      startPeriod = 15
    }
  }])
}

# ---- ECS Service -----------------------------------------------------------

resource "aws_ecs_service" "server" {
  name            = var.app_name
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.server.arn
  desired_count   = var.conduit_desired_count
  launch_type     = "FARGATE"

  # Rolling update: keep at least 50 % healthy during deploys.
  deployment_minimum_healthy_percent = 50
  deployment_maximum_percent         = 200

  network_configuration {
    subnets          = aws_subnet.private[*].id
    security_groups  = [aws_security_group.ecs.id]
    assign_public_ip = false # tasks are in private subnets; NAT for egress
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.server.arn
    container_name   = var.app_name
    container_port   = 8080
  }

  # Ignore desired_count changes in Terraform after initial deploy so that
  # auto-scaling policies (if added later) can manage the count freely.
  lifecycle {
    ignore_changes = [desired_count]
  }

  depends_on = [
    aws_lb_listener.http,
    aws_iam_role_policy_attachment.ecs_execution,
  ]
}

# ---- Auto-scaling ----------------------------------------------------------

resource "aws_appautoscaling_target" "server" {
  max_capacity       = 10
  min_capacity       = var.conduit_desired_count
  resource_id        = "service/${aws_ecs_cluster.main.name}/${aws_ecs_service.server.name}"
  scalable_dimension = "ecs:service:DesiredCount"
  service_namespace  = "ecs"
}

# Scale out when average CPU > 60 % for 2 consecutive minutes.
resource "aws_appautoscaling_policy" "cpu" {
  name               = "${var.app_name}-cpu"
  policy_type        = "TargetTrackingScaling"
  resource_id        = aws_appautoscaling_target.server.resource_id
  scalable_dimension = aws_appautoscaling_target.server.scalable_dimension
  service_namespace  = aws_appautoscaling_target.server.service_namespace

  target_tracking_scaling_policy_configuration {
    predefined_metric_specification {
      predefined_metric_type = "ECSServiceAverageCPUUtilization"
    }
    target_value       = 60
    scale_in_cooldown  = 300
    scale_out_cooldown = 60
  }
}
