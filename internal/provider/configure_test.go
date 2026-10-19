package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

// configure runs the provider's Configure with a given block, and returns the
// resulting client (nil when configuration failed).
func configure(t *testing.T, block map[string]tftypes.Value) (*nemau.Client, string) {
	t.Helper()
	ctx := context.Background()
	p := New("test")()

	var schemaResp provider.SchemaResponse
	p.Schema(ctx, provider.SchemaRequest{}, &schemaResp)

	objectType := tftypes.Object{AttributeTypes: map[string]tftypes.Type{
		"api_url":           tftypes.String,
		"access_key_id":     tftypes.String,
		"access_key_secret": tftypes.String,
		"token":             tftypes.String,
		"project":           tftypes.String,
	}}
	for name := range objectType.AttributeTypes {
		if _, ok := block[name]; !ok {
			block[name] = tftypes.NewValue(tftypes.String, nil)
		}
	}

	req := provider.ConfigureRequest{
		Config: tfsdk.Config{
			Schema: schemaResp.Schema,
			Raw:    tftypes.NewValue(objectType, block),
		},
	}
	var resp provider.ConfigureResponse
	p.Configure(ctx, req, &resp)

	messages := make([]string, 0, len(resp.Diagnostics))
	for _, d := range resp.Diagnostics {
		messages = append(messages, d.Summary()+": "+d.Detail())
	}
	joined := strings.Join(messages, "\n")

	if resp.ResourceData == nil {
		return nil, joined
	}
	client, ok := resp.ResourceData.(*nemau.Client)
	if !ok {
		t.Fatalf("ResourceData is %T, want *nemau.Client", resp.ResourceData)
	}
	return client, joined
}

// known wraps a concrete string for the config object under test.
func known(value string) tftypes.Value { return tftypes.NewValue(tftypes.String, value) }

func TestConfigureWithAccessKey(t *testing.T) {
	// Clear the environment so the test only depends on the provider block.
	t.Setenv(nemau.EnvAccessKeyID, "")
	t.Setenv(nemau.EnvAccessKeySecret, "")
	t.Setenv(nemau.EnvToken, "")
	t.Setenv(nemau.EnvAPIURL, "")
	t.Setenv(nemau.EnvProject, "")

	client, diags := configure(t, map[string]tftypes.Value{
		"api_url":           known("https://api.example.test"),
		"access_key_id":     known("AKIA"),
		"access_key_secret": known("s3cr3t"),
	})
	if client == nil {
		t.Fatalf("configuration refused: %s", diags)
	}
	if client.BaseURL() != "https://api.example.test" {
		t.Errorf("baseURL = %q", client.BaseURL())
	}
	if got := client.DescribeCredential(); got != "access-key AKIA" {
		t.Errorf("credential = %q", got)
	}
}

func TestConfigureFallsBackToEnvironment(t *testing.T) {
	t.Setenv(nemau.EnvAccessKeyID, "")
	t.Setenv(nemau.EnvAccessKeySecret, "")
	t.Setenv(nemau.EnvToken, "env-token")
	t.Setenv(nemau.EnvAPIURL, "https://env.example.test")
	t.Setenv(nemau.EnvProject, "proj-from-env")

	client, diags := configure(t, map[string]tftypes.Value{})
	if client == nil {
		t.Fatalf("configuration refused: %s", diags)
	}
	if client.BaseURL() != "https://env.example.test" {
		t.Errorf("baseURL = %q", client.BaseURL())
	}
	if got := client.DescribeCredential(); got != "service-account token" {
		t.Errorf("credential = %q", got)
	}
	if client.Project() != "proj-from-env" {
		t.Errorf("project = %q", client.Project())
	}
}

func TestConfigureProjectBlockBeatsEnvironment(t *testing.T) {
	t.Setenv(nemau.EnvAccessKeyID, "")
	t.Setenv(nemau.EnvAccessKeySecret, "")
	t.Setenv(nemau.EnvToken, "env-token")
	t.Setenv(nemau.EnvProject, "proj-from-env")

	client, diags := configure(t, map[string]tftypes.Value{"project": known("proj-from-block")})
	if client == nil {
		t.Fatalf("configuration refused: %s", diags)
	}
	if client.Project() != "proj-from-block" {
		t.Errorf("project = %q, want the block to win over the environment", client.Project())
	}
}

func TestConfigureWithoutProjectLeavesTheCredentialInCharge(t *testing.T) {
	// Setting no project is valid. The credential's own project applies, and
	// the SDK sends no project header.
	t.Setenv(nemau.EnvAccessKeyID, "")
	t.Setenv(nemau.EnvAccessKeySecret, "")
	t.Setenv(nemau.EnvToken, "env-token")
	t.Setenv(nemau.EnvProject, "")

	client, diags := configure(t, map[string]tftypes.Value{})
	if client == nil {
		t.Fatalf("configuration refused: %s", diags)
	}
	if client.Project() != "" {
		t.Errorf("project = %q, want empty", client.Project())
	}
}

func TestConfigureRejectsHalfACredential(t *testing.T) {
	t.Setenv(nemau.EnvAccessKeyID, "")
	t.Setenv(nemau.EnvAccessKeySecret, "")
	t.Setenv(nemau.EnvToken, "")

	// An access key id without its secret is a configuration error, and the
	// message must name the missing half.
	_, diags := configure(t, map[string]tftypes.Value{"access_key_id": known("AKIA")})
	if !strings.Contains(diags, "access_key_secret") {
		t.Errorf("diagnostic = %q, must name access_key_secret", diags)
	}
}

func TestConfigureRejectsNoCredential(t *testing.T) {
	t.Setenv(nemau.EnvAccessKeyID, "")
	t.Setenv(nemau.EnvAccessKeySecret, "")
	t.Setenv(nemau.EnvToken, "")

	_, diags := configure(t, map[string]tftypes.Value{})
	if !strings.Contains(diags, "No credential") {
		t.Errorf("diagnostic = %q", diags)
	}
}

func TestConfigureRejectsUnknownValues(t *testing.T) {
	t.Setenv(nemau.EnvToken, "")
	// An unknown value comes from another resource's output. Terraform cannot
	// configure a provider from it, and the message must name the attribute.
	_, diags := configure(t, map[string]tftypes.Value{
		"token": tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
	})
	if !strings.Contains(diags, "Unknown provider configuration") || !strings.Contains(diags, nemau.EnvToken) {
		t.Errorf("diagnostic = %q", diags)
	}
}

// strList maps a nil slice to an empty list rather than null, so a practitioner
// can index into it without a null check.
func TestStrListNeverNull(t *testing.T) {
	empty := strList(nil)
	if empty.IsNull() || empty.IsUnknown() {
		t.Fatalf("strList(nil) = %v, want an empty list", empty)
	}
	if len(empty.Elements()) != 0 {
		t.Errorf("strList(nil) holds %d elements", len(empty.Elements()))
	}

	filled := strList([]string{"10.0.0.1", "10.0.0.2"})
	if got := len(filled.Elements()); got != 2 {
		t.Errorf("strList produced %d elements", got)
	}
}

// str maps "" to null so an unset optional attribute does not read back as an
// empty string and produce a perpetual diff.
func TestStrMapsEmptyToNull(t *testing.T) {
	if !str("").IsNull() {
		t.Error(`str("") must be null`)
	}
	if str("x") != types.StringValue("x") {
		t.Error(`str("x") must be "x"`)
	}
}
