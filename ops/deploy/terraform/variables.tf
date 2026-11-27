variable "aws_region" {
  type    = string
  default = "eu-west-3"
}

variable "project" {
  type    = string
  default = "indexus-aws"
}

variable "network_id" {
  type    = string
  default = "indexus-aws"
}

variable "vpc_id" {
  type = string
}

variable "public_subnet_id" {
  type = string
}

variable "instance_type" {
  type    = string
  default = "t3.micro"
}

variable "ami_id" {
  type        = string
  default     = ""
  description = "Pre-baked Indexus node AMI for spawned instances (see ops/deploy/scripts/bake-ami.sh)."
}

variable "bootstrap_ami_id" {
  type        = string
  default     = ""
  description = "Optional AMI for the bootstrap instance. Empty = stock AL2023 (userdata installs issuer+node)."
}

variable "worker_count" {
  type        = number
  default     = 0
  description = "Static TF workers. Prefer 0 and let bootstrap autoscale spawn via launch template."
}

variable "spawn_max" {
  type        = number
  default     = 9
  description = "Cost cap on concurrent spawned instances, not a target mesh size."
}

variable "admin_cidrs" {
  type    = list(string)
  default = ["0.0.0.0/0"]
}

variable "ssh_public_key" {
  type    = string
  default = ""
}

variable "existing_key_name" {
  type    = string
  default = ""
}

variable "tags" {
  type = map(string)
  default = {
    Project = "indexus-aws"
  }
}
