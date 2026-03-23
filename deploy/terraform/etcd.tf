# ---- etcd EC2 node ---------------------------------------------------------
#
# This is a single-node etcd deployment, which is fine for a PoC or
# low-volume production service. For high availability, promote to a
# 3-node cluster by:
#   1. Adding 2 more aws_instance resources with peer URLs configured.
#   2. Passing --initial-cluster-state=new with all three peer URLs.
#   3. Using a Network Load Balancer in front of port 2379.
#
# etcd is placed in a private subnet with no public IP. Only ECS tasks can
# reach it via the sg_etcd security group rule.

resource "aws_instance" "etcd" {
  ami                    = data.aws_ami.al2023.id
  instance_type          = var.etcd_instance_type
  subnet_id              = aws_subnet.private[0].id
  vpc_security_group_ids = [aws_security_group.etcd.id]

  # No SSH key — in production, use SSM Session Manager for shell access.
  # Uncomment the next line if you need SSH for debugging:
  # key_name = "your-key-pair"

  root_block_device {
    volume_type           = "gp3"
    volume_size           = 20
    delete_on_termination = true
    encrypted             = true
  }

  user_data = base64encode(templatefile("${path.module}/etcd-init.sh.tpl", {
    etcd_version = var.etcd_version
  }))

  tags = { Name = "${var.app_name}-etcd" }
}

# ---- etcd init script (rendered from template at plan time) ----------------
# The actual file is written below using local_file. Alternatively this can
# be an external templatefile reference; keeping it inline reduces file count.

resource "local_file" "etcd_init_tpl" {
  filename = "${path.module}/etcd-init.sh.tpl"
  content  = <<-'TMPL'
#!/bin/bash
set -euo pipefail

ETCD_VER="${etcd_version}"
ARCH="linux-amd64"
DOWNLOAD_URL="https://github.com/etcd-io/etcd/releases/download"

# Install etcd
cd /tmp
curl -sL "$${DOWNLOAD_URL}/$${ETCD_VER}/etcd-$${ETCD_VER}-$${ARCH}.tar.gz" \
  | tar xz
mv "etcd-$${ETCD_VER}-$${ARCH}/etcd" /usr/local/bin/
mv "etcd-$${ETCD_VER}-$${ARCH}/etcdctl" /usr/local/bin/

# Data directory
mkdir -p /var/lib/etcd
chmod 700 /var/lib/etcd

# Private IP of this instance (used as the advertise address).
PRIVATE_IP=$(TOKEN=$(curl -sX PUT "http://169.254.169.254/latest/api/token" \
  -H "X-aws-ec2-metadata-token-ttl-seconds: 60") && \
  curl -s -H "X-aws-ec2-metadata-token: $TOKEN" \
  http://169.254.169.254/latest/meta-data/local-ipv4)

# systemd unit
cat > /etc/systemd/system/etcd.service << EOF
[Unit]
Description=etcd key-value store
Documentation=https://etcd.io/docs/
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
User=root
ExecStart=/usr/local/bin/etcd \
  --name=conduit-etcd \
  --data-dir=/var/lib/etcd \
  --listen-client-urls=http://0.0.0.0:2379 \
  --advertise-client-urls=http://$${PRIVATE_IP}:2379 \
  --auto-compaction-retention=1
Restart=always
RestartSec=5
LimitNOFILE=40000

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable etcd
systemctl start etcd
TMPL
}
