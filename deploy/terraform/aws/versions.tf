terraform {
  required_version = ">= 1.9.0, < 2.0.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.82"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "~> 4.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }

  backend "s3" {
    # Values supplied by `terraform init -backend-config=backends/<env>.hcl`.
    encrypt = true
    # Native S3 locking (Terraform 1.9+) rather than a DynamoDB table: one
    # fewer resource to create before the first apply and one fewer thing to
    # forget when bootstrapping a new account.
    use_lockfile = true
  }
}

provider "aws" {
  region = var.region

  # Every apply is tagged with its origin. When an unexpected resource turns up
  # in a cost report, the first question is always "which pipeline made this".
  default_tags {
    tags = {
      application      = "agentgate"
      managed_by       = "terraform"
      terraform_module = "deploy/terraform/aws"
    }
  }

  # Guard against pointing a production apply at the wrong account, which is
  # the single most expensive mistake available in this directory.
  allowed_account_ids = [var.account_id]
}

# us-east-1 alias: CloudFront and some global services only accept certificates
# and WAF ACLs from this region. Declared even where unused so adding an edge
# component later does not require a provider change mid-review.
provider "aws" {
  alias  = "us_east_1"
  region = "us-east-1"

  allowed_account_ids = [var.account_id]
}
