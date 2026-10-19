# Terraform Provider for Nemau Cloud

The Nemau Cloud provider manages instances, networks, load balancers, volumes,
object storage, managed Kubernetes clusters and DNS zones on
[Nemau Cloud](https://nemau-cloud.com).

```hcl
terraform {
  required_providers {
    nemau = {
      source  = "nemau-cloud/nemau"
      version = "~> 0.1"
    }
  }
}

provider "nemau" {}
```

The provider reads its credential from `NEMAU_ACCESS_KEY_ID` and
`NEMAU_ACCESS_KEY_SECRET`, or from `NEMAU_TOKEN` for a service-account token.
`NEMAU_PROJECT_ID` selects the project and `NEMAU_API_URL` overrides the API
origin.

## Documentation

- [Provider documentation on the Terraform Registry](https://registry.terraform.io/providers/nemau-cloud/nemau/latest/docs)
- [The same pages in this repository](docs)
- [Terraform on the Nemau Cloud documentation site](https://docs.nemau-cloud.com/en/tools/terraform)
- [Changelog](CHANGELOG.md)
- [Contributing guide](CONTRIBUTING.md)

## Examples

Each folder under [examples](examples) holds a complete configuration:

- [complete](examples/complete): network, subnet, security group, instance,
  volume and floating IP
- [kubernetes](examples/kubernetes): a managed cluster with a second node pool
- [immutable-backups](examples/immutable-backups): containers with object lock
- [multi-project](examples/multi-project): two projects with one credential
- [dns](examples/dns): a zone, the proof of its domain's ownership, and a record
- [https-load-balancer](examples/https-load-balancer): HTTPS terminated on a
  load balancer, and certificate renewal
- [container-credential](examples/container-credential): S3 keys scoped to one
  container

## Getting help

Report a bug or ask a question in the
[issue tracker](https://github.com/nemau-cloud/terraform-provider-nemau/issues).
For questions about your account, write to contact@nemau-cloud.com.

## Security

Do not report vulnerabilities in public issues. See [SECURITY.md](SECURITY.md).

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
