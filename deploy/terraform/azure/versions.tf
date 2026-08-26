terraform {
  # 1.9 is the floor because this configuration uses provider-defined functions
  # and `terraform test` fixtures in CI. Pinning the lower bound stops a
  # developer on an older CLI from producing a plan that silently ignores
  # newer validation blocks.
  required_version = ">= 1.9.0, < 2.0.0"

  required_providers {
    azurerm = {
      source = "hashicorp/azurerm"
      # 4.x is a major rewrite of resource defaults; pinning to the minor
      # keeps `terraform init` reproducible while still taking patch fixes.
      version = "~> 4.14"
    }
    azuread = {
      source  = "hashicorp/azuread"
      version = "~> 3.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
    time = {
      source  = "hashicorp/time"
      version = "~> 0.12"
    }
  }

  # State lives in a storage account created out of band, with versioning and
  # a 30-day soft delete. Bootstrapping state with the same configuration it
  # stores is a circular dependency nobody enjoys resolving at 02:00.
  backend "azurerm" {
    # Values supplied by `terraform init -backend-config=backends/<env>.hcl`
    # so one configuration serves dev, staging and prod without a workspace
    # sprawl or a copied directory.
    use_azuread_auth = true
  }
}

provider "azurerm" {
  features {
    key_vault {
      # Never purge on destroy. A destroyed Key Vault that took the token
      # signing key with it is unrecoverable, and `terraform destroy` in the
      # wrong directory is a thing that happens.
      purge_soft_delete_on_destroy          = false
      purge_soft_deleted_keys_on_destroy    = false
      purge_soft_deleted_secrets_on_destroy = false
      recover_soft_deleted_key_vaults       = true
      recover_soft_deleted_keys             = true
      recover_soft_deleted_secrets          = true
    }
    cognitive_account {
      purge_soft_delete_on_destroy = false
    }
    resource_group {
      # Refuse to delete a resource group that still contains resources
      # Terraform does not know about — usually something a human created
      # during an incident and forgot.
      prevent_deletion_if_contains_resources = true
    }
  }
  storage_use_azuread = true
  subscription_id     = var.subscription_id
}

provider "azuread" {
  tenant_id = var.tenant_id
}
