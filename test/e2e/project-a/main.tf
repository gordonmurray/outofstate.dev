# End-to-end test project A. Created and destroyed by test/e2e/run.sh.
terraform {
  required_providers {
    aws    = { source = "hashicorp/aws", version = "~> 6.72" }
    random = { source = "hashicorp/random", version = "~> 3.7" }
  }
}

variable "run_id" {
  type = string
}

provider "aws" {
  default_tags {
    tags = { oos-e2e = var.run_id }
  }
}

# The password lives in state on purpose: oos must never print it.
resource "random_password" "db" {
  length  = 24
  special = false
}

resource "aws_db_instance" "a1" {
  identifier              = "oos-e2e-${var.run_id}-a1"
  engine                  = "postgres"
  instance_class          = "db.t4g.micro"
  allocated_storage       = 20
  username                = "oos"
  password                = random_password.db.result
  skip_final_snapshot     = true
  backup_retention_period = 0
  apply_immediately       = true
}
