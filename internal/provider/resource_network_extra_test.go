package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

// validate runs a resource's ValidateConfig against a configuration whose
// unlisted attributes are null, and returns the diagnostics as text.
func validate(t *testing.T, r resource.ResourceWithValidateConfig, values map[string]tftypes.Value) string {
	t.Helper()
	ctx := context.Background()
	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	objectType, ok := schemaResp.Schema.Type().TerraformType(ctx).(tftypes.Object)
	if !ok {
		t.Fatal("schema type is not an object")
	}
	block := map[string]tftypes.Value{}
	for name, attributeType := range objectType.AttributeTypes {
		if value, set := values[name]; set {
			block[name] = value
		} else {
			block[name] = tftypes.NewValue(attributeType, nil)
		}
	}
	var resp resource.ValidateConfigResponse
	r.ValidateConfig(ctx, resource.ValidateConfigRequest{Config: tfsdk.Config{
		Schema: schemaResp.Schema, Raw: tftypes.NewValue(objectType, block),
	}}, &resp)
	messages := make([]string, 0, len(resp.Diagnostics))
	for _, d := range resp.Diagnostics {
		messages = append(messages, d.Summary()+": "+d.Detail())
	}
	return strings.Join(messages, "\n")
}

func TestSubnetConfigShapeFollowsTheIPVersion(t *testing.T) {
	name := tftypes.NewValue(tftypes.String, "app")
	network := tftypes.NewValue(tftypes.String, "net-1")
	cases := []struct {
		label  string
		values map[string]tftypes.Value
		want   string // "" = valid
	}{
		{"ipv4 with a range", map[string]tftypes.Value{"cidr": tftypes.NewValue(tftypes.String, "10.0.0.0/24")}, ""},
		{"ipv4 without a range", map[string]tftypes.Value{}, "Missing CIDR"},
		{"ipv6 alone", map[string]tftypes.Value{"ip_version": tftypes.NewValue(tftypes.Number, 6)}, ""},
		{"ipv6 with a range", map[string]tftypes.Value{
			"ip_version": tftypes.NewValue(tftypes.Number, 6),
			"cidr":       tftypes.NewValue(tftypes.String, "2001:db8::/64"),
		}, "Remove `cidr`"},
		{"ipv6 with a gateway", map[string]tftypes.Value{
			"ip_version": tftypes.NewValue(tftypes.Number, 6),
			"gateway_ip": tftypes.NewValue(tftypes.String, "2001:db8::1"),
		}, "Remove `gateway_ip`"},
		{"unknown version", map[string]tftypes.Value{
			"ip_version": tftypes.NewValue(tftypes.Number, 5),
			"cidr":       tftypes.NewValue(tftypes.String, "10.0.0.0/24"),
		}, "must be 4 or 6"},
		// A version coming from another resource cannot be checked yet.
		{"version not known until apply", map[string]tftypes.Value{
			"ip_version": tftypes.NewValue(tftypes.Number, tftypes.UnknownValue),
		}, ""},
	}
	for _, tc := range cases {
		tc.values["name"], tc.values["network_id"] = name, network
		got := validate(t, NewSubnetResource().(resource.ResourceWithValidateConfig), tc.values)
		if tc.want == "" && got != "" {
			t.Errorf("%s: unexpected diagnostics: %s", tc.label, got)
		}
		if tc.want != "" && !strings.Contains(got, tc.want) {
			t.Errorf("%s: diagnostics %q, want %q", tc.label, got, tc.want)
		}
	}
}

func TestFloatingIPVersionIsValidated(t *testing.T) {
	r := NewFloatingIPResource().(resource.ResourceWithValidateConfig)
	if got := validate(t, r, map[string]tftypes.Value{"ip_version": tftypes.NewValue(tftypes.Number, 6)}); got != "" {
		t.Fatalf("ip_version = 6 refused: %s", got)
	}
	if got := validate(t, r, map[string]tftypes.Value{}); got != "" {
		t.Fatalf("an omitted ip_version refused: %s", got)
	}
	if got := validate(t, r, map[string]tftypes.Value{"ip_version": tftypes.NewValue(tftypes.Number, 5)}); !strings.Contains(got, "must be 4 or 6") {
		t.Fatalf("ip_version = 5 accepted: %q", got)
	}
}

// The version, the range and the gateway identify the subnet. A change must
// plan a replacement, and an omitted value keeps what the API assigned instead
// of planning a replacement on every unrelated change.
func TestIPv6AttributesKeepStateAndForceReplacement(t *testing.T) {
	ctx := context.Background()
	schemaOf := func(r resource.Resource) fwresource.Schema {
		var resp resource.SchemaResponse
		r.Schema(ctx, resource.SchemaRequest{}, &resp)
		return resp.Schema
	}
	check := func(label string, optional, computed bool, descriptions []string) {
		t.Helper()
		if !optional || !computed {
			t.Errorf("%s must be Optional and Computed", label)
		}
		var replaces, keepsState bool
		for _, description := range descriptions {
			description = strings.ToLower(description)
			replaces = replaces || strings.Contains(description, "destroy and recreate")
			keepsState = keepsState || strings.Contains(description, "will not change")
		}
		if !replaces || !keepsState {
			t.Errorf("%s plan modifiers: replace=%v keepState=%v", label, replaces, keepsState)
		}
	}
	int64Attribute := func(s fwresource.Schema, name string) {
		attr, ok := s.Attributes[name].(fwresource.Int64Attribute)
		if !ok {
			t.Fatalf("%s missing", name)
		}
		var descriptions []string
		for _, modifier := range attr.PlanModifiers {
			descriptions = append(descriptions, modifier.Description(ctx))
		}
		check(name, attr.Optional, attr.Computed, descriptions)
	}
	stringAttribute := func(s fwresource.Schema, name string) {
		attr, ok := s.Attributes[name].(fwresource.StringAttribute)
		if !ok {
			t.Fatalf("%s missing", name)
		}
		var descriptions []string
		for _, modifier := range attr.PlanModifiers {
			descriptions = append(descriptions, modifier.Description(ctx))
		}
		check(name, attr.Optional, attr.Computed, descriptions)
	}

	subnet := schemaOf(NewSubnetResource())
	int64Attribute(subnet, "ip_version")
	stringAttribute(subnet, "cidr")
	stringAttribute(subnet, "gateway_ip")
	int64Attribute(schemaOf(NewFloatingIPResource()), "ip_version")
}

func TestApplyReadsTheIPVersion(t *testing.T) {
	var subnet subnetModel
	applySubnet(&subnet, &nemau.Subnet{ID: "sub-6", CIDR: "2001:db8:100:1::/64", GatewayIP: "2001:db8:100:1::1", IPVersion: nemau.IPv6})
	if subnet.IPVersion.ValueInt64() != 6 || subnet.CIDR.ValueString() != "2001:db8:100:1::/64" {
		t.Fatalf("subnet = %+v", subnet)
	}
	// A region that predates IPv6 omits the field: everything it serves is IPv4.
	applySubnet(&subnet, &nemau.Subnet{ID: "sub-4", CIDR: "10.0.0.0/24"})
	if subnet.IPVersion.ValueInt64() != 4 {
		t.Fatalf("an omitted version read as %d", subnet.IPVersion.ValueInt64())
	}

	var fip floatingIPModel
	applyFloatingIP(&fip, &nemau.FloatingIP{ID: "fip-6", Address: "2001:db8:f::10", IPVersion: nemau.IPv6})
	if fip.IPVersion.ValueInt64() != 6 {
		t.Fatalf("floating ip = %+v", fip)
	}
	applyFloatingIP(&fip, &nemau.FloatingIP{ID: "fip-4", Address: "203.0.113.7"})
	if fip.IPVersion.ValueInt64() != 4 {
		t.Fatalf("an omitted version read as %d", fip.IPVersion.ValueInt64())
	}
}

func TestContainerRetentionIsCappedAtSevenYears(t *testing.T) {
	config := func(days int64) map[string]tftypes.Value {
		return map[string]tftypes.Value{
			"name":           tftypes.NewValue(tftypes.String, "backups"),
			"object_lock":    tftypes.NewValue(tftypes.Bool, true),
			"retention_mode": tftypes.NewValue(tftypes.String, "COMPLIANCE"),
			"retention_days": tftypes.NewValue(tftypes.Number, days),
		}
	}
	r := NewContainerResource().(resource.ResourceWithValidateConfig)
	if got := validate(t, r, config(2555)); got != "" {
		t.Errorf("2555 days was refused: %s", got)
	}
	if got := validate(t, r, config(2556)); !strings.Contains(got, "must not exceed 2555") {
		t.Errorf("2556 days was admitted: %q", got)
	}
}
