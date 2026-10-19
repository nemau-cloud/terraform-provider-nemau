# Two projects, one credential.
#
# The `alias` configurations differ only in `project`: the organization comes
# from the access key, and the project from the argument, as with gcloud and
# scw.

terraform {
  required_version = ">= 1.5"
  required_providers {
    nemau = {
      source  = "nemau-cloud/nemau"
      version = "~> 0.1"
    }
  }
}

# Credential read from NEMAU_ACCESS_KEY_ID / NEMAU_ACCESS_KEY_SECRET, shared by
# both configurations below.
provider "nemau" {
  alias   = "staging"
  project = var.staging_project_id
}

provider "nemau" {
  alias   = "prod"
  project = var.prod_project_id
}

resource "nemau_network" "staging" {
  provider    = nemau.staging
  name        = "app"
  description = "Staging application network"
}

resource "nemau_network" "prod" {
  provider    = nemau.prod
  name        = "app"
  description = "Production application network"
}

# Two networks with the same name coexist without conflict: they live in two
# different projects, and name uniqueness is per project.

resource "nemau_subnet" "staging" {
  provider   = nemau.staging
  network_id = nemau_network.staging.id
  name       = "app"
  cidr       = "10.10.0.0/24"
}

resource "nemau_subnet" "prod" {
  provider   = nemau.prod
  network_id = nemau_network.prod.id
  name       = "app"
  cidr       = "10.20.0.0/24"
}

output "staging_network_id" {
  value = nemau_network.staging.id
}

output "prod_network_id" {
  value = nemau_network.prod.id
}
