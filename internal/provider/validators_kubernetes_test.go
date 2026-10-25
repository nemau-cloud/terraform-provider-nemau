package provider

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// stringErrors runs the attribute's string validators on value.
func stringErrors(t *testing.T, validators []validator.String, value types.String) []string {
	t.Helper()
	var out []string
	for _, v := range validators {
		var resp validator.StringResponse
		v.ValidateString(context.Background(), validator.StringRequest{
			Path: path.Root("attr"), ConfigValue: value,
		}, &resp)
		for _, d := range resp.Diagnostics.Errors() {
			out = append(out, d.Detail())
		}
	}
	return out
}

// listErrors runs the attribute's list validators on the entries.
func listErrors(t *testing.T, validators []validator.List, entries []string) []string {
	t.Helper()
	elements := make([]attr.Value, 0, len(entries))
	for _, e := range entries {
		elements = append(elements, types.StringValue(e))
	}
	value := types.ListValueMust(types.StringType, elements)
	var out []string
	for _, v := range validators {
		var resp validator.ListResponse
		v.ValidateList(context.Background(), validator.ListRequest{
			Path: path.Root("attr"), ConfigValue: value,
		}, &resp)
		for _, d := range resp.Diagnostics.Errors() {
			out = append(out, d.Detail())
		}
	}
	return out
}

func TestUUIDValidator(t *testing.T) {
	v := []validator.String{validUUID("network_id")}
	for _, ok := range []string{exampleNetworkID, strings.ToUpper(exampleNetworkID)} {
		if errs := stringErrors(t, v, types.StringValue(ok)); len(errs) != 0 {
			t.Errorf("%q refused: %v", ok, errs)
		}
	}
	for _, bad := range []string{"", "not-a-uuid", "3f2b8c1e5d4a4e7b9c1d0a1b2c3d4e5f", "3f2b8c1e-5d4a-4e7b-9c1d-0a1b2c3d4e5fx"} {
		if errs := stringErrors(t, v, types.StringValue(bad)); len(errs) == 0 || !strings.Contains(errs[0], "`network_id`") {
			t.Errorf("%q accepted or message does not name the attribute: %v", bad, errs)
		}
	}
	if errs := stringErrors(t, v, types.StringUnknown()); len(errs) != 0 {
		t.Errorf("unknown value validated: %v", errs)
	}
}

func TestIPv4CIDRValidator(t *testing.T) {
	v := []validator.String{ipv4CIDR("pod_cidr")}
	for _, ok := range []string{"10.244.0.0/16", "10.96.0.0/12"} {
		if errs := stringErrors(t, v, types.StringValue(ok)); len(errs) != 0 {
			t.Errorf("%q refused: %v", ok, errs)
		}
	}
	for _, bad := range []string{"10.244.0.1", "2001:db8::/32", "10.0.0.0/33", "not-a-cidr", ""} {
		if errs := stringErrors(t, v, types.StringValue(bad)); len(errs) == 0 {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestNodePoolLabelValidation(t *testing.T) {
	for _, ok := range []string{"team=platform", "gpu=true", "zone=eu-west-1", "a=b", "tier=1.2_x"} {
		if err := checkNodePoolLabel(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"team",                            // no value
		"team=",                           // empty value
		"Team=x",                          // uppercase key
		"team/x=y",                        // prefixed keys are not accepted
		"team=-x",                         // value must start with a letter or digit
		"team=x y",                        // space
		"team=" + strings.Repeat("a", 64), // value over 63 characters
		strings.Repeat("k", 64) + "=v",    // key over 63 characters
	} {
		if err := checkNodePoolLabel(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestNodePoolTaintValidation(t *testing.T) {
	for _, ok := range []string{"dedicated=gpu:NoSchedule", "dedicated:NoExecute", "spot=true:PreferNoSchedule"} {
		if err := checkNodePoolTaint(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"dedicated=gpu",              // no effect
		"dedicated=gpu:noschedule",   // effect is case sensitive
		"dedicated=gpu:NoSchedule:x", // extra segment
		"Dedicated:NoSchedule",       // uppercase key
		"dedicated=:NoExecute",       // empty value
		"dedicated=gpu:Evict",        // unknown effect
	} {
		if err := checkNodePoolTaint(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestNodePoolListValidators(t *testing.T) {
	labels := []validator.List{nodePoolLabels()}
	taints := []validator.List{nodePoolTaints()}

	if errs := listErrors(t, labels, []string{"a=b", "c=d"}); len(errs) != 0 {
		t.Errorf("valid labels refused: %v", errs)
	}
	if errs := listErrors(t, taints, []string{"dedicated=gpu:NoSchedule"}); len(errs) != 0 {
		t.Errorf("valid taints refused: %v", errs)
	}

	sixteen := make([]string, maxNodePoolLabels)
	seventeen := make([]string, maxNodePoolLabels+1)
	for i := range seventeen {
		seventeen[i] = fmt.Sprintf("k%d=v", i)
		if i < len(sixteen) {
			sixteen[i] = seventeen[i]
		}
	}
	if errs := listErrors(t, labels, sixteen); len(errs) != 0 {
		t.Errorf("16 labels refused: %v", errs)
	}
	if errs := listErrors(t, labels, seventeen); len(errs) == 0 || !strings.Contains(strings.Join(errs, "|"), "at most 16") {
		t.Errorf("17 labels accepted: %v", errs)
	}
	if errs := listErrors(t, taints, seventeen); len(errs) == 0 {
		// Those entries are labels, not taints: they must fail as taints too.
		t.Errorf("17 entries that are not taints accepted")
	}
	if errs := listErrors(t, labels, []string{"bad"}); len(errs) == 0 || !strings.Contains(errs[0], "key=value") {
		t.Errorf("invalid label not reported: %v", errs)
	}
}

func TestNodePoolClassIsDefaultWorker(t *testing.T) {
	v := []validator.String{nodePoolClassOneOf()}
	if errs := stringErrors(t, v, types.StringValue("default-worker")); len(errs) != 0 {
		t.Errorf("default-worker refused: %v", errs)
	}
	for _, bad := range []string{"gpu-worker", "", "Default-Worker"} {
		if errs := stringErrors(t, v, types.StringValue(bad)); len(errs) == 0 {
			t.Errorf("class %q accepted", bad)
		}
	}
}

// The network, subnet and CIDRs are required or computed as the contract says.
func TestNetworkAttributesRequiredness(t *testing.T) {
	_, s := clusterResource(t, nil)
	for _, name := range []string{"network_id", "subnet_id"} {
		a := s.Attributes[name].(schema.StringAttribute)
		if !a.Required || a.Optional || a.Computed {
			t.Errorf("%s: want Required only", name)
		}
	}
	for _, name := range []string{"pod_cidr", "service_cidr"} {
		a := s.Attributes[name].(schema.StringAttribute)
		if a.Required || !a.Optional || !a.Computed {
			t.Errorf("%s: want Optional and Computed", name)
		}
	}
}

// The plan modifier, on a real request: a changed UUID on an existing cluster
// requires replacement, and so does a value the plan leaves unknown, which may
// differ once applied. Only a create and an unchanged value do not.
func TestNetworkIDPlanModifier(t *testing.T) {
	_, s := clusterResource(t, nil)
	attrs := s.Attributes["network_id"].(schema.StringAttribute)
	modifiers := attrs.PlanModifiers

	existing := tfsdk.State{Raw: tftypes.NewValue(tftypes.String, exampleNetworkID)}
	created := tfsdk.State{Raw: tftypes.NewValue(tftypes.String, nil)}
	run := func(state tfsdk.State, stateValue, planValue types.String) bool {
		t.Helper()
		// A non-null plan: the framework's RequiresReplace never replaces a destroy.
		plan := tfsdk.Plan{Raw: tftypes.NewValue(tftypes.String, "planned")}
		req := planmodifier.StringRequest{State: state, Plan: plan, StateValue: stateValue, PlanValue: planValue}
		var resp planmodifier.StringResponse
		resp.PlanValue = planValue
		for _, m := range modifiers {
			m.PlanModifyString(context.Background(), req, &resp)
		}
		return resp.RequiresReplace
	}
	if !run(existing, types.StringValue(exampleNetworkID), types.StringValue(exampleSubnetID)) {
		t.Error("changed network on an existing cluster does not require replacement")
	}
	if run(existing, types.StringValue(exampleNetworkID), types.StringValue(exampleNetworkID)) {
		t.Error("unchanged network requires replacement")
	}
	if !run(existing, types.StringValue(exampleNetworkID), types.StringUnknown()) {
		t.Error("unknown network on an existing cluster does not require replacement")
	}
	if run(created, types.StringNull(), types.StringValue(exampleNetworkID)) {
		t.Error("create requires replacement")
	}
}
