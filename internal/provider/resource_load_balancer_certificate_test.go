package provider

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

func replacesOnChange(modifiers interface{ Description(context.Context) string }) bool {
	return strings.Contains(strings.ToLower(modifiers.Description(context.Background())), "destroy and recreate")
}

// A certificate is immutable on the platform: every configured attribute
// replaces it, and the private key is sensitive.
func TestLoadBalancerCertificateIsImmutableAndItsKeySensitive(t *testing.T) {
	var resp resource.SchemaResponse
	NewLoadBalancerCertificateResource().Schema(context.Background(), resource.SchemaRequest{}, &resp)
	for _, name := range []string{"name", "name_prefix", "certificate", "chain", "private_key"} {
		attr, ok := resp.Schema.Attributes[name].(fwresource.StringAttribute)
		if !ok {
			t.Fatalf("%s attribute missing", name)
		}
		replaces := false
		for _, modifier := range attr.PlanModifiers {
			replaces = replaces || replacesOnChange(modifier)
		}
		if !replaces {
			t.Errorf("%s must force a replacement", name)
		}
	}
	key := resp.Schema.Attributes["private_key"].(fwresource.StringAttribute)
	if !key.Sensitive || !key.Required {
		t.Fatalf("private_key must be required and sensitive, got required=%v sensitive=%v", key.Required, key.Sensitive)
	}
	// No import: an imported certificate would have no private key to plan with.
	if _, importable := NewLoadBalancerCertificateResource().(resource.ResourceWithImportState); importable {
		t.Fatal("nemau_load_balancer_certificate must not be importable")
	}
}

// Read fills what the API reports and leaves the PEM attributes, which it
// never returns, as they are in state.
func TestApplyLoadBalancerCertificateKeepsThePEMFromState(t *testing.T) {
	model := loadBalancerCertificateModel{
		Certificate: types.StringValue("CERT"),
		PrivateKey:  types.StringValue("KEY"),
		Chain:       types.StringNull(),
	}
	applyLoadBalancerCertificate(&model, &nemau.Certificate{
		ID: "crt-1", Domains: []string{"www.example.com"}, Issuer: "Example CA (R11)",
		NotAfter: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), KeyAlgorithm: "ECDSA-P256",
	})
	if model.PrivateKey.ValueString() != "KEY" || model.Certificate.ValueString() != "CERT" || !model.Chain.IsNull() {
		t.Fatalf("PEM attributes were overwritten: %+v", model)
	}
	var domains []string
	model.Domains.ElementsAs(context.Background(), &domains, false)
	if model.ID.ValueString() != "crt-1" || model.NotAfter.ValueString() != "2026-12-01T00:00:00Z" ||
		len(domains) != 1 || !model.NotBefore.IsNull() {
		t.Fatalf("metadata = %+v", model)
	}
}

// Repointing a listener is how a certificate is renewed: the reference must
// change in place, not recreate the listener.
func TestListenerCertificateChangesInPlace(t *testing.T) {
	var resp resource.SchemaResponse
	NewListenerResource().Schema(context.Background(), resource.SchemaRequest{}, &resp)
	attr, ok := resp.Schema.Attributes["tls_certificate_ref"].(fwresource.StringAttribute)
	if !ok {
		t.Fatal("tls_certificate_ref attribute missing")
	}
	for _, modifier := range attr.PlanModifiers {
		if replacesOnChange(modifier) {
			t.Fatal("changing a listener's certificate must not recreate the listener")
		}
	}
}

func TestLoadBalancerCertificateTypeName(t *testing.T) {
	var resp resource.MetadataResponse
	NewLoadBalancerCertificateResource().Metadata(context.Background(), resource.MetadataRequest{ProviderTypeName: "nemau"}, &resp)
	if resp.TypeName != "nemau_load_balancer_certificate" {
		t.Fatalf("type name = %q", resp.TypeName)
	}
}

// validateCertificateConfig runs ValidateConfig on a configuration where only
// the given string attributes are set.
func validateCertificateConfig(t *testing.T, set map[string]string) []string {
	t.Helper()
	ctx := context.Background()
	r := NewLoadBalancerCertificateResource().(*loadBalancerCertificateResource)
	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	objectType := schemaResp.Schema.Type().TerraformType(ctx).(tftypes.Object)
	values := map[string]tftypes.Value{}
	for name, attrType := range objectType.AttributeTypes {
		if value, ok := set[name]; ok {
			values[name] = tftypes.NewValue(attrType, value)
		} else {
			values[name] = tftypes.NewValue(attrType, nil)
		}
	}
	req := resource.ValidateConfigRequest{Config: tfsdk.Config{Schema: schemaResp.Schema, Raw: tftypes.NewValue(objectType, values)}}
	var resp resource.ValidateConfigResponse
	r.ValidateConfig(ctx, req, &resp)
	var summaries []string
	for _, d := range resp.Diagnostics.Errors() {
		summaries = append(summaries, d.Summary())
	}
	return summaries
}

// Exactly one of name and name_prefix: a renewal with create_before_destroy
// needs a new name each time, which name_prefix generates.
func TestLoadBalancerCertificateNameOrPrefix(t *testing.T) {
	base := map[string]string{"certificate": "CERT", "private_key": "KEY"}
	with := func(extra map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	if errs := validateCertificateConfig(t, with(map[string]string{"name": "www"})); len(errs) != 0 {
		t.Fatalf("name alone refused: %v", errs)
	}
	if errs := validateCertificateConfig(t, with(map[string]string{"name_prefix": "www-"})); len(errs) != 0 {
		t.Fatalf("name_prefix alone refused: %v", errs)
	}
	if errs := validateCertificateConfig(t, with(map[string]string{"name": "www", "name_prefix": "www-"})); len(errs) != 1 {
		t.Fatalf("name and name_prefix together accepted: %v", errs)
	}
	if errs := validateCertificateConfig(t, base); len(errs) != 1 {
		t.Fatalf("neither name nor name_prefix accepted: %v", errs)
	}
	if errs := validateCertificateConfig(t, with(map[string]string{"name_prefix": strings.Repeat("a", maxNamePrefixLength+1)})); len(errs) != 1 {
		t.Fatalf("an oversized prefix was accepted: %v", errs)
	}
}

func TestGeneratedCertificateNamesAreUniqueAndFit(t *testing.T) {
	first, err := generateCertificateName("www-")
	if err != nil {
		t.Fatal(err)
	}
	second, _ := generateCertificateName("www-")
	if !regexp.MustCompile(`^www-\d{14}[0-9a-f]{8}$`).MatchString(first) || first == second {
		t.Fatalf("generated names %q and %q", first, second)
	}
	longest, _ := generateCertificateName(strings.Repeat("a", maxNamePrefixLength))
	if len(longest) != maxCertificateNameLength {
		t.Fatalf("longest generated name is %d characters", len(longest))
	}
}

// Read keeps the generated name the API reports, so a plan does not see it
// unknown and replace the certificate.
func TestNameIsKeptFromStateOnceGenerated(t *testing.T) {
	var resp resource.SchemaResponse
	NewLoadBalancerCertificateResource().Schema(context.Background(), resource.SchemaRequest{}, &resp)
	name := resp.Schema.Attributes["name"].(fwresource.StringAttribute)
	if !name.Optional || !name.Computed {
		t.Fatal("name must be optional and computed")
	}
	keep := stringplanmodifier.UseStateForUnknown().Description(context.Background())
	if name.PlanModifiers[0].Description(context.Background()) != keep {
		t.Fatal("name must keep its state value before RequiresReplace compares it")
	}
	model := loadBalancerCertificateModel{Name: types.StringUnknown()}
	applyLoadBalancerCertificate(&model, &nemau.Certificate{ID: "crt-1", Name: "www-2026092312000012ab34cd"})
	if model.Name.ValueString() != "www-2026092312000012ab34cd" {
		t.Fatalf("name = %v", model.Name)
	}
}
