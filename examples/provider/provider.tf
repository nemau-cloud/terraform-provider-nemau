terraform {
  required_providers {
    nemau = {
      source  = "nemau-cloud/nemau"
      version = "~> 0.1"
    }
  }
}

# The credential comes from NEMAU_ACCESS_KEY_ID and NEMAU_ACCESS_KEY_SECRET,
# and the project from NEMAU_PROJECT_ID or from the credential.
provider "nemau" {}
