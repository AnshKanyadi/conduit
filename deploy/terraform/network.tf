# ---- VPC -------------------------------------------------------------------

resource "aws_vpc" "main" {
  cidr_block           = "10.0.0.0/16"
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = { Name = "${var.app_name}-vpc" }
}

# Public subnets (ALB lives here — must be publicly routable).
resource "aws_subnet" "public" {
  count                   = var.az_count
  vpc_id                  = aws_vpc.main.id
  cidr_block              = cidrsubnet("10.0.0.0/16", 8, count.index + 1)
  availability_zone       = data.aws_availability_zones.available.names[count.index]
  map_public_ip_on_launch = true

  tags = { Name = "${var.app_name}-public-${count.index + 1}" }
}

# Private subnets (ECS tasks and etcd — no direct internet access).
resource "aws_subnet" "private" {
  count             = var.az_count
  vpc_id            = aws_vpc.main.id
  cidr_block        = cidrsubnet("10.0.0.0/16", 8, count.index + 11)
  availability_zone = data.aws_availability_zones.available.names[count.index]

  tags = { Name = "${var.app_name}-private-${count.index + 1}" }
}

resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id
  tags   = { Name = "${var.app_name}-igw" }
}

# NAT Gateway (in the first public subnet) — allows ECS tasks to pull ECR
# images and reach etcd, without a public IP.
resource "aws_eip" "nat" {
  domain = "vpc"
}

resource "aws_nat_gateway" "main" {
  allocation_id = aws_eip.nat.id
  subnet_id     = aws_subnet.public[0].id
  tags          = { Name = "${var.app_name}-nat" }
  depends_on    = [aws_internet_gateway.main]
}

# Route tables.
resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.main.id
  }
  tags = { Name = "${var.app_name}-rt-public" }
}

resource "aws_route_table_association" "public" {
  count          = var.az_count
  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

resource "aws_route_table" "private" {
  vpc_id = aws_vpc.main.id
  route {
    cidr_block     = "0.0.0.0/0"
    nat_gateway_id = aws_nat_gateway.main.id
  }
  tags = { Name = "${var.app_name}-rt-private" }
}

resource "aws_route_table_association" "private" {
  count          = var.az_count
  subnet_id      = aws_subnet.private[count.index].id
  route_table_id = aws_route_table.private.id
}

# ---- Security groups -------------------------------------------------------

# ALB: accepts HTTP from the internet; forwards to ECS.
resource "aws_security_group" "alb" {
  name        = "${var.app_name}-alb"
  description = "Allow HTTP inbound to the ALB"
  vpc_id      = aws_vpc.main.id

  ingress {
    description = "HTTP"
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  egress {
    description = "ALB → ECS"
    from_port   = 8080
    to_port     = 8080
    protocol    = "tcp"
    cidr_blocks = [aws_vpc.main.cidr_block]
  }
}

# ECS tasks: accept traffic from ALB; reach etcd; pull from ECR via NAT.
resource "aws_security_group" "ecs" {
  name        = "${var.app_name}-ecs"
  description = "Conduit ECS task"
  vpc_id      = aws_vpc.main.id

  ingress {
    description     = "ALB → server"
    from_port       = 8080
    to_port         = 8080
    protocol        = "tcp"
    security_groups = [aws_security_group.alb.id]
  }

  egress {
    description = "All outbound (ECR, etcd, internet)"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

# etcd: only reachable from ECS tasks inside the VPC.
resource "aws_security_group" "etcd" {
  name        = "${var.app_name}-etcd"
  description = "etcd client API (port 2379) — ECS access only"
  vpc_id      = aws_vpc.main.id

  ingress {
    description     = "ECS → etcd client API"
    from_port       = 2379
    to_port         = 2379
    protocol        = "tcp"
    security_groups = [aws_security_group.ecs.id]
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

# ---- Application Load Balancer ---------------------------------------------

resource "aws_lb" "main" {
  name               = var.app_name
  internal           = false
  load_balancer_type = "application"
  security_groups    = [aws_security_group.alb.id]
  subnets            = aws_subnet.public[*].id

  # WebSocket connections can be hours long; default 60 s would drop them.
  idle_timeout = 3600
}

resource "aws_lb_target_group" "server" {
  name        = "${var.app_name}-server"
  port        = 8080
  protocol    = "HTTP"
  vpc_id      = aws_vpc.main.id
  target_type = "ip" # Fargate tasks register by IP, not instance ID.

  health_check {
    path                = "/metrics"
    interval            = 30
    timeout             = 5
    healthy_threshold   = 2
    unhealthy_threshold = 3
    matcher             = "200"
  }

  # Cookie-based sticky sessions so a client consistently hits the same
  # Conduit task — avoids round-trip redirects via the consensus layer for
  # the common reconnect case.
  stickiness {
    type            = "lb_cookie"
    cookie_duration = 86400 # 1 day
    enabled         = true
  }
}

resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.main.arn
  port              = 80
  protocol          = "HTTP"

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.server.arn
  }
}
