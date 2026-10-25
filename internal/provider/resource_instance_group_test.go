package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

// fixtureUserDataSecret stands in for a secret in a document. It is never real.
const fixtureUserDataSecret = "FIXTURESECRETVALUE"

func TestUserDataRulesNameTheRuleWithoutEchoingTheDocument(t *testing.T) {
	for _, ok := range []string{
		"#cloud-config\npackages: [curl]\n",
		"#!/bin/sh\necho hi\n",
		"\uFEFF#cloud-config\n",
		"Content-Type: multipart/mixed; boundary=x\n",
	} {
		if err := checkUserDataDocument(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}

	refused := map[string]string{
		"no header": fixtureUserDataSecret,
		"blank":     "   \n",
		"nul byte":  "#cloud-config\n\x00",
		"bad utf-8": "#cloud-config\n\xff",
		"oversized": "#cloud-config\n" + strings.Repeat("a", groupMaxUserData),
	}
	for name, doc := range refused {
		err := checkUserDataDocument(doc)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), fixtureUserDataSecret) {
			t.Errorf("%s: the error echoes the document: %v", name, err)
		}
	}
}

func TestPoliciesKeepThePriorTuningWhenThePlanIsUnknown(t *testing.T) {
	ctx := context.Background()
	var diags diag.Diagnostics
	policyList := func(items ...groupPolicyModel) types.List {
		list, d := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: groupPolicyAttrTypes}, items)
		diags.Append(d...)
		return list
	}
	prior := policyList(groupPolicyModel{
		Metric: types.StringValue("cpu_percent"), Target: types.Int64Value(60),
		WindowSeconds: types.Int64Value(300), ScaleInEnabled: types.BoolValue(false),
		ScaleOutCooldownSeconds: types.Int64Value(120), ScaleInCooldownSeconds: types.Int64Value(300),
		ScaleInWindowSeconds: types.Int64Value(600), MaxStepOut: types.Int64Value(2), MaxStepIn: types.Int64Value(1),
	})
	plan := policyList(groupPolicyModel{
		Metric: types.StringValue("cpu_percent"), Target: types.Int64Value(70),
		WindowSeconds: types.Int64Unknown(), ScaleOutCooldownSeconds: types.Int64Unknown(),
		ScaleInCooldownSeconds: types.Int64Unknown(), ScaleInWindowSeconds: types.Int64Unknown(),
		MaxStepOut: types.Int64Unknown(), MaxStepIn: types.Int64Unknown(), ScaleInEnabled: types.BoolUnknown(),
	})

	got := policiesFromList(ctx, plan, prior, &diags)
	if diags.HasError() || len(got) != 1 {
		t.Fatalf("policies = %+v, %v", got, diags)
	}
	p := got[0]
	if p.Target != 70 || p.WindowSeconds != 300 || p.MaxStepIn != 1 || p.ScaleInWindowSeconds != 600 {
		t.Errorf("unknown planned values must keep the prior ones: %+v", p)
	}
	if p.ScaleInEnabled == nil || *p.ScaleInEnabled {
		t.Errorf("scale_in_enabled = %v, want the prior false", p.ScaleInEnabled)
	}
}

func TestTemplateChangeComparesOnlyWhatTheConfigSets(t *testing.T) {
	ctx := context.Background()
	state := objectOf(t, launchTemplateModel{
		ImageID: types.StringValue("debian-12"), FlavorID: types.StringValue("small"),
		VCPUs: types.Int64Value(2), RAMMB: types.Int64Value(2048), DiskGB: types.Int64Value(20),
		UserData: types.StringValue("#cloud-config\n"),
	})

	// Only image_id is set: flavor and sizing come from the group and do not count.
	same := objectOf(t, launchTemplateModel{
		ImageID: types.StringValue("debian-12"), FlavorID: types.StringNull(), VCPUs: types.Int64Null(),
		RAMMB: types.Int64Null(), DiskGB: types.Int64Null(), KeyName: types.StringNull(),
		ServiceLevel: types.StringNull(), UserData: types.StringValue("#cloud-config\n"),
	})
	if changed, d := templateChanged(ctx, same, state); d.HasError() || changed {
		t.Errorf("unchanged template reported as changed (%v)", d)
	}

	newImage := objectOf(t, launchTemplateModel{
		ImageID: types.StringValue("debian-13"), FlavorID: types.StringNull(), VCPUs: types.Int64Null(),
		RAMMB: types.Int64Null(), DiskGB: types.Int64Null(), KeyName: types.StringNull(),
		ServiceLevel: types.StringNull(), UserData: types.StringValue("#cloud-config\n"),
	})
	if changed, _ := templateChanged(ctx, newImage, state); !changed {
		t.Error("a new image was not reported")
	}

	newDoc := objectOf(t, launchTemplateModel{
		ImageID: types.StringValue("debian-12"), FlavorID: types.StringNull(), VCPUs: types.Int64Null(),
		RAMMB: types.Int64Null(), DiskGB: types.Int64Null(), KeyName: types.StringNull(),
		ServiceLevel: types.StringNull(), UserData: types.StringValue("#!/bin/sh\n"),
	})
	if changed, _ := templateChanged(ctx, newDoc, state); !changed {
		t.Error("a changed user_data was not reported")
	}
}

func TestFillGroupKeepsTheUserDataTheAPIDoesNotReturn(t *testing.T) {
	ctx := context.Background()
	prior := objectOf(t, launchTemplateModel{
		ImageID: types.StringValue("debian-12"), FlavorID: types.StringNull(), VCPUs: types.Int64Null(),
		RAMMB: types.Int64Null(), DiskGB: types.Int64Null(), KeyName: types.StringNull(),
		ServiceLevel: types.StringNull(), UserData: types.StringValue("#cloud-config\n"),
	})
	m := instanceGroupModel{LaunchTemplate: prior, SecurityGroups: types.ListNull(types.StringType)}
	g := &nemau.InstanceGroup{
		ID: "g-1", Name: "web-pool", Status: "READY", MinSize: 1, MaxSize: 4, DesiredCapacity: 2,
		HealthCheck: "instance", LaunchTemplate: nemau.LaunchTemplateInfo{
			ImageID: "debian-12", FlavorID: "small", VCPUs: 2, RAMMB: 2048, DiskGB: 20, UserDataPresent: true,
		},
	}
	if d := fillGroup(ctx, &m, g, false); d.HasError() {
		t.Fatalf("fill: %v", d)
	}
	var tmpl launchTemplateModel
	if d := m.LaunchTemplate.As(ctx, &tmpl, basetypes.ObjectAsOptions{}); d.HasError() {
		t.Fatal(d)
	}
	if tmpl.UserData.ValueString() != "#cloud-config\n" {
		t.Errorf("user_data = %v; the read must keep the configured document", tmpl.UserData)
	}
	if tmpl.FlavorID.ValueString() != "small" || tmpl.VCPUs.ValueInt64() != 2 {
		t.Errorf("template not filled from the API: %+v", tmpl)
	}
	if !m.LoadBalancerPools.IsNull() && len(m.LoadBalancerPools.Elements()) != 0 {
		t.Errorf("no pools reported, but %d pools kept", len(m.LoadBalancerPools.Elements()))
	}
}

func TestSameValueAndNonNil(t *testing.T) {
	if !sameValue[string](nil, []string{}) {
		t.Error("nil and empty must compare equal")
	}
	on := true
	a := []nemau.ScalingPolicy{{Metric: "cpu_percent", Target: 60, ScaleInEnabled: &on}}
	b := []nemau.ScalingPolicy{{Metric: "cpu_percent", Target: 60, ScaleInEnabled: func() *bool { v := true; return &v }()}}
	if !sameValue(a, b) {
		t.Error("equal policies with different pointers must compare equal")
	}
	if sameValue(a, []nemau.ScalingPolicy{{Metric: "cpu_percent", Target: 61}}) {
		t.Error("different policies compared equal")
	}
	if got := nonNil[string](nil); got == nil || len(got) != 0 {
		t.Error("nonNil must return an empty, non-nil list")
	}
}

func TestSecurityGroupsReadAsNullWhenNoneWasSet(t *testing.T) {
	m := instanceGroupModel{SecurityGroups: types.ListNull(types.StringType)}
	var d diag.Diagnostics
	m.LoadBalancerPools = listFromPools(context.Background(), nil, &d)
	if m.LoadBalancerPools.IsNull() {
		t.Error("an empty pool block list must be an empty list, not null")
	}
	if got := optionalStrList(nil, m.SecurityGroups); !got.IsNull() {
		t.Error("no groups with a null prior must stay null")
	}
}

// objectOf builds the template object for a model.
func objectOf(t *testing.T, m launchTemplateModel) types.Object {
	t.Helper()
	obj, d := types.ObjectValueFrom(context.Background(), launchTemplateAttrTypes, m)
	if d.HasError() {
		t.Fatal(d)
	}
	return obj
}
