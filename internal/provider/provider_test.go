package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	fwdatasource "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource/schema"
)

// The framework validates a schema when it builds one; these tests exercise
// every Schema() so a malformed attribute is caught here rather than by
// `terraform plan` on a practitioner's machine.

func TestResourceSchemasAreValid(t *testing.T) {
	ctx := context.Background()
	p := New("test")().(*nemauProvider)

	var metadata provider.MetadataResponse
	p.Metadata(ctx, provider.MetadataRequest{}, &metadata)

	seen := map[string]bool{}
	for _, factory := range p.Resources(ctx) {
		res := factory()

		var meta resource.MetadataResponse
		res.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: metadata.TypeName}, &meta)
		if !strings.HasPrefix(meta.TypeName, "nemau_") {
			t.Errorf("resource type %q does not start with nemau_", meta.TypeName)
		}
		if seen[meta.TypeName] {
			t.Errorf("resource type %q declared twice", meta.TypeName)
		}
		seen[meta.TypeName] = true

		var schemaResp resource.SchemaResponse
		res.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
		if schemaResp.Diagnostics.HasError() {
			t.Fatalf("%s: invalid schema: %v", meta.TypeName, schemaResp.Diagnostics)
		}
		assertResourceSchema(t, meta.TypeName, schemaResp.Schema)
	}

	// Check the names as well as the count, so that a resource removed from
	// Resources() by mistake fails here.
	for _, want := range []string{
		"nemau_instance", "nemau_keypair", "nemau_snapshot",
		"nemau_network", "nemau_subnet", "nemau_floating_ip",
		"nemau_floating_ip_association", "nemau_security_group",
		"nemau_security_group_rule", "nemau_volume", "nemau_volume_attachment",
		"nemau_container", "nemau_container_credential", "nemau_load_balancer", "nemau_listener", "nemau_pool",
		"nemau_pool_member", "nemau_health_monitor", "nemau_dns_zone",
		"nemau_dns_zone_verification", "nemau_dns_record_set", "nemau_kubernetes_cluster", "nemau_kubernetes_node_pool",
		"nemau_database", "nemau_instance_group",
	} {
		if !seen[want] {
			t.Errorf("resource %s is missing from the provider", want)
		}
	}
}

func assertResourceSchema(t *testing.T, name string, s fwresource.Schema) {
	t.Helper()

	// Every resource needs an id: Terraform's import and state addressing rely
	// on it, and a resource without one cannot be imported at all.
	id, ok := s.Attributes["id"]
	if !ok {
		t.Errorf("%s: id attribute missing", name)
		return
	}
	if !id.IsComputed() {
		t.Errorf("%s: id must be Computed", name)
	}

	if s.MarkdownDescription == "" {
		t.Errorf("%s: schema has no description; the registry would publish an empty page", name)
	}

	for attrName, attr := range s.Attributes {
		if !attr.IsRequired() && !attr.IsOptional() && !attr.IsComputed() {
			t.Errorf("%s.%s: neither Required, Optional nor Computed", name, attrName)
		}
		if attr.IsRequired() && attr.IsComputed() {
			t.Errorf("%s.%s: Required and Computed are incompatible", name, attrName)
		}
	}

	assertDescribed(t, name, s.Attributes)
	for blockName, block := range s.Blocks {
		if block.GetMarkdownDescription() == "" {
			t.Errorf("%s.%s: block has no description", name, blockName)
		}
		if nested, ok := block.(fwresource.SingleNestedBlock); ok {
			assertDescribed(t, name+"."+blockName, nested.Attributes)
		}
	}
}

// assertDescribed fails for an attribute without a description. The registry
// pages under docs/ are generated from the schema, so an undescribed attribute
// is published with nothing but its type.
func assertDescribed[A interface{ GetMarkdownDescription() string }](t *testing.T, prefix string, attributes map[string]A) {
	t.Helper()
	for attrName, attr := range attributes {
		if attr.GetMarkdownDescription() == "" {
			t.Errorf("%s.%s: attribute has no description", prefix, attrName)
		}
	}
}

func TestDataSourceSchemasAreValid(t *testing.T) {
	ctx := context.Background()
	p := New("test")().(*nemauProvider)

	seen := map[string]bool{}
	for _, factory := range p.DataSources(ctx) {
		ds := factory()

		var meta datasource.MetadataResponse
		ds.Metadata(ctx, datasource.MetadataRequest{ProviderTypeName: "nemau"}, &meta)
		if seen[meta.TypeName] {
			t.Errorf("data source %q declared twice", meta.TypeName)
		}
		seen[meta.TypeName] = true

		var schemaResp datasource.SchemaResponse
		ds.Schema(ctx, datasource.SchemaRequest{}, &schemaResp)
		if schemaResp.Diagnostics.HasError() {
			t.Fatalf("%s: invalid schema: %v", meta.TypeName, schemaResp.Diagnostics)
		}
		if schemaResp.Schema.MarkdownDescription == "" {
			t.Errorf("%s: schema has no description", meta.TypeName)
		}
		assertDataSourceSchema(t, meta.TypeName, schemaResp.Schema)
	}

	for _, want := range []string{
		"nemau_images", "nemau_flavors", "nemau_regions", "nemau_networks",
		"nemau_instance", "nemau_kubernetes_versions", "nemau_kubeconfig",
		"nemau_database", "nemau_instance_group", "nemau_database_flavors", "nemau_database_versions",
	} {
		if !seen[want] {
			t.Errorf("data source %s missing from the provider", want)
		}
	}
}

func assertDataSourceSchema(t *testing.T, name string, s fwdatasource.Schema) {
	t.Helper()
	for attrName, attr := range s.Attributes {
		if !attr.IsRequired() && !attr.IsOptional() && !attr.IsComputed() {
			t.Errorf("%s.%s: neither Required, Optional nor Computed", name, attrName)
		}
		if nested, ok := attr.(fwdatasource.ListNestedAttribute); ok {
			assertDescribed(t, name+"."+attrName, nested.NestedObject.Attributes)
		}
	}
	assertDescribed(t, name, s.Attributes)
}

func TestProviderSchemaMarksSecretsSensitive(t *testing.T) {
	ctx := context.Background()
	p := New("test")()

	var resp provider.SchemaResponse
	p.Schema(ctx, provider.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("invalid provider schema: %v", resp.Diagnostics)
	}

	// A credential that is not marked sensitive is printed by `terraform plan`
	// and lands in CI logs.
	for _, name := range []string{"access_key_secret", "token"} {
		attr, ok := resp.Schema.Attributes[name]
		if !ok {
			t.Fatalf("provider attribute %s missing", name)
		}
		if !attr.IsSensitive() {
			t.Errorf("%s must be Sensitive", name)
		}
	}
	if resp.Schema.Attributes["access_key_id"].IsSensitive() {
		t.Error("access_key_id is not a secret; marking it sensitive hides a useful diagnostic")
	}
}

// The registry documentation is generated from each type's schema and example
// (see CONTRIBUTING.md). A type registered without an example would be
// published without one, and a stale docs/ would miss the type altogether.
func TestEveryTypeHasDocsAndExample(t *testing.T) {
	ctx := context.Background()
	p := New("test")()
	root := filepath.Join("..", "..")

	check := func(kind, typeName, example string) {
		t.Helper()
		short := strings.TrimPrefix(typeName, "nemau_")
		for _, path := range []string{
			filepath.Join(root, "examples", kind, typeName, example),
			filepath.Join(root, "docs", kind, short+".md"),
		} {
			if _, err := os.Stat(path); err != nil {
				t.Errorf("%s: %s is missing; add the example and run `go generate ./...`", typeName, path)
			}
		}
	}

	for _, factory := range p.Resources(ctx) {
		var meta resource.MetadataResponse
		factory().Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "nemau"}, &meta)
		check("resources", meta.TypeName, "resource.tf")
	}
	for _, factory := range p.DataSources(ctx) {
		var meta datasource.MetadataResponse
		factory().Metadata(ctx, datasource.MetadataRequest{ProviderTypeName: "nemau"}, &meta)
		check("data-sources", meta.TypeName, "data-source.tf")
	}
}
