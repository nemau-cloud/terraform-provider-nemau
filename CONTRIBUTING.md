# Contributing to the Nemau Cloud provider

Bug reports, fixes and improvements are welcome.

## Proposing a change

Report a bug or suggest a change in the
[issue tracker](https://github.com/nemau-cloud/terraform-provider-nemau/issues).
For a large change, open an issue first so the approach can be discussed before
you write the code.

Send changes as a pull request against `main`. Keep each pull request to one
topic, and include tests for new behaviour.

Do not report security issues in the tracker. See [SECURITY.md](SECURITY.md).

## Building and testing

The provider needs Go 1.27 or later. Before sending a change, run:

```sh
go build ./... && go vet ./... && go test ./... && gofmt -l .
```

`gofmt -l .` must print nothing. CI also runs the tests with `-race`, and checks
the examples with `terraform fmt` and `terraform validate`.

`make build` writes `bin/terraform-provider-nemau` with the version taken from
the latest git tag.

The provider is built on [nemau-sdk-go](https://github.com/nemau-cloud/nemau-sdk-go).
Requests to the API and the handling of its responses belong in the SDK; the
provider maps them onto Terraform resources. To change both at once, clone the
two repositories side by side and create a `go.work` file in their parent
directory:

```
go 1.27.0

use (
	./nemau-sdk-go
	./terraform-provider-nemau
)
```

Go then builds the provider against your copy of the SDK, and `go.mod` needs no
`replace`.

## Running the provider locally

To try a build without going through the registry, run `make build` and add a
`dev_overrides` block to `~/.terraformrc`:

```hcl
provider_installation {
  dev_overrides {
    "nemau-cloud/nemau" = "/path/to/terraform-provider-nemau/bin"
  }
  direct {}
}
```

Terraform then uses the binary in `bin` for `nemau-cloud/nemau` and prints a warning
on every command. `terraform init` is not needed for this provider, but it still
is for any other provider the configuration uses. Remove the block to go back
to the registry version.

## Documentation

The pages under [docs](docs) are generated with
[tfplugindocs](https://github.com/hashicorp/terraform-plugin-docs) and published
on the Terraform Registry. Do not edit them by hand. They are built from:

- the `MarkdownDescription` of each resource, data source and attribute in the
  Go schema, under `internal/provider`;
- the templates under [templates](templates): the provider page, the guides,
  and one layout shared by every resource and one by every data source;
- the examples: `examples/provider/provider.tf`, and for each type a folder
  named after it under `examples/resources` or `examples/data-sources`,
  holding `resource.tf` or `data-source.tf`, plus `import.sh` when the
  resource can be imported.

After changing any of them, regenerate the pages from the repository root and
commit the result with the change:

```sh
go generate ./...
```

The generator reads the schema through Terraform. It uses the `terraform`
binary on your `PATH`, or downloads one into a temporary directory. CI fails
when the committed pages are out of date.

Every attribute needs a description; `go test ./...` fails otherwise. A new
resource or data source also needs its example folder.

Documentation, descriptions, messages and examples are written in English.

## Commit messages

Write a short subject line that starts with a capital letter and has no
trailing period, such as `Describe the retention limit of a container`. When
the reason for the change is not obvious, explain it in the body after a blank
line.

## Changelog

Add a line for every user-visible change to [CHANGELOG.md](CHANGELOG.md), under
the `Unreleased` heading, in the section that fits, such as `Added`, `Changed`
or `Fixed`. The file follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
