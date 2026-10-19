// Command terraform-provider-nemau is the Terraform provider for Nemau Cloud.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/nemau-cloud/terraform-provider-nemau/internal/provider"
)

// The registry documentation under docs/ is generated from the provider schema,
// templates/ and examples/. Run `go generate ./...` after changing any of them.
//go:generate go run github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs@v0.25.0 generate --provider-name nemau

// version is stamped at build time by goreleaser or the Makefile.
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run the provider with support for debuggers like delve")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		// The registry address. Terraform resolves `nemau-cloud/nemau` to this binary
		// through a dev override or the registry itself.
		Address: "registry.terraform.io/nemau-cloud/nemau",
		Debug:   debug,
	})
	if err != nil {
		log.Fatal(err.Error())
	}
}
