# Disposable experiment stack: `hosts` machines, each running one agent with
# `slots` node slots (host 0 also runs the issuer and the provisioner), plus
# one load machine that drives the whole campaign through the agents.
# Separate state from ops/deploy: it never touches the live deployment. Every
# instance terminates itself after ttl_minutes even if nobody destroys it.
terraform {
  required_version = ">= 1.5.0"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

variable "region" { default = "eu-west-3" }
variable "run_id" { description = "Unique run name, e.g. exp-20261115-1400" }
variable "hosts" { default = 4 }
variable "slots" { default = 8 }
variable "host_type" { default = "c6i.2xlarge" }
variable "loader_type" { default = "c6i.2xlarge" }
variable "data_bytes" { default = 4294967296 }
variable "sizes" { default = "8 16 32" }
variable "mode" {
  default     = "all"
  description = "all: every phase; push: the ladder, raising writes/s per size until saturation"
}
variable "items_per_vnode" { default = 20000 }
variable "ttl_minutes" { default = 120 }
variable "bin_dir" { description = "Directory holding linux/amd64 node, issuer, provisioner, experiment and elastic/*.sh" }
variable "plan" { description = "Campaign plan run by the loader (scenarios.sh)" }
variable "subnet_id" {
  default     = ""
  description = "Public subnet; empty uses the default VPC's first default subnet"
}

provider "aws" {
  region = var.region
  default_tags {
    tags = { Project = "indexus-experiment", Run = var.run_id }
  }
}

data "aws_ami" "al2023" {
  most_recent = true
  owners      = ["amazon"]
  filter {
    name   = "name"
    values = ["al2023-ami-2023.*-x86_64"]
  }
}

data "aws_subnets" "default" {
  count = var.subnet_id == "" ? 1 : 0
  filter {
    name   = "default-for-az"
    values = ["true"]
  }
}

data "aws_subnet" "chosen" {
  id = var.subnet_id != "" ? var.subnet_id : sort(data.aws_subnets.default[0].ids)[0]
}

resource "aws_s3_bucket" "run" {
  bucket_prefix = "indexus-exp-"
  force_destroy = true
}

resource "aws_s3_object" "bin" {
  for_each = toset(["node", "issuer", "provisioner", "experiment", "elastic/spawn.sh", "elastic/count.sh", "elastic/terminate.sh", "ca/ca.crt", "ca/ca.key"])
  bucket   = aws_s3_bucket.run.id
  key      = "bin/${each.key}"
  source   = "${var.bin_dir}/${each.key}"
  etag     = filemd5("${var.bin_dir}/${each.key}")
}

resource "aws_s3_object" "plan" {
  bucket = aws_s3_bucket.run.id
  key    = "plan/scenarios.sh"
  source = var.plan
  etag   = filemd5(var.plan)
}

# Members talk to each other on any port; nothing is reachable from outside.
resource "aws_security_group" "exp" {
  name_prefix = "indexus-exp-"
  vpc_id      = data.aws_subnet.chosen.vpc_id
  ingress {
    from_port = 0
    to_port   = 0
    protocol  = "-1"
    self      = true
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_iam_role" "exp" {
  name_prefix = "indexus-exp-"
  assume_role_policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Principal = { Service = "ec2.amazonaws.com" }, Action = "sts:AssumeRole" }]
  })
}

# Read binaries, write results and the agent list; nothing else.
resource "aws_iam_role_policy" "exp" {
  role = aws_iam_role.exp.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      { Effect = "Allow", Action = ["s3:GetObject", "s3:PutObject", "s3:ListBucket"], Resource = [aws_s3_bucket.run.arn, "${aws_s3_bucket.run.arn}/*"] },
    ]
  })
}

resource "aws_iam_instance_profile" "exp" {
  name_prefix = "indexus-exp-"
  role        = aws_iam_role.exp.name
}

locals {
  common = {
    data_storage = "auto"
    bucket      = aws_s3_bucket.run.id
    region      = var.region
    ttl         = var.ttl_minutes
    slots       = var.slots
    total_slots = var.hosts * var.slots
    data_bytes  = var.data_bytes
  }
}

resource "aws_instance" "host0" {
  ami                                  = data.aws_ami.al2023.id
  instance_type                        = var.host_type
  subnet_id                            = data.aws_subnet.chosen.id
  vpc_security_group_ids               = [aws_security_group.exp.id]
  iam_instance_profile                 = aws_iam_instance_profile.exp.name
  associate_public_ip_address          = true
  instance_initiated_shutdown_behavior = "terminate"
  user_data                            = templatefile("${path.module}/host.sh.tftpl", merge(local.common, { index = 0, host0 = "" }))
  root_block_device {
    volume_type = "gp3"
    volume_size = 40
  }
  tags       = { Name = "${var.run_id}-h0", Role = "host" }
  depends_on = [aws_s3_object.bin]
}

resource "aws_instance" "host" {
  count                                = var.hosts - 1
  ami                                  = data.aws_ami.al2023.id
  instance_type                        = var.host_type
  subnet_id                            = data.aws_subnet.chosen.id
  vpc_security_group_ids               = [aws_security_group.exp.id]
  iam_instance_profile                 = aws_iam_instance_profile.exp.name
  associate_public_ip_address          = true
  instance_initiated_shutdown_behavior = "terminate"
  user_data                            = templatefile("${path.module}/host.sh.tftpl", merge(local.common, { index = count.index + 1, host0 = aws_instance.host0.private_ip }))
  root_block_device {
    volume_type = "gp3"
    volume_size = 40
  }
  tags = { Name = "${var.run_id}-h${count.index + 1}", Role = "host" }
}

resource "aws_instance" "loader" {
  ami                                  = data.aws_ami.al2023.id
  instance_type                        = var.loader_type
  subnet_id                            = data.aws_subnet.chosen.id
  vpc_security_group_ids               = [aws_security_group.exp.id]
  iam_instance_profile                 = aws_iam_instance_profile.exp.name
  associate_public_ip_address          = true
  instance_initiated_shutdown_behavior = "terminate"
  user_data = templatefile("${path.module}/loader.sh.tftpl", merge(local.common, {
    sizes = var.sizes, items = var.items_per_vnode, mode = var.mode,
    ips   = join(" ", concat([aws_instance.host0.private_ip], aws_instance.host[*].private_ip))
  }))
  tags       = { Name = "${var.run_id}-loader", Role = "loader" }
  depends_on = [aws_s3_object.plan]
}

output "bucket" { value = aws_s3_bucket.run.id }
