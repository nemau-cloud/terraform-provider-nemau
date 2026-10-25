package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

// backupBlock builds a backup_policy block value. A field left out of fields
// is null, as in a configuration that does not set it.
func backupBlock(t *testing.T, s schema.Schema, fields map[string]tftypes.Value) tftypes.Value {
	t.Helper()
	objType := s.Type().TerraformType(context.Background()).(tftypes.Object).AttributeTypes["backup_policy"].(tftypes.Object)
	values := map[string]tftypes.Value{}
	for name, attrType := range objType.AttributeTypes {
		values[name] = tftypes.NewValue(attrType, nil)
	}
	for k, v := range fields {
		values[k] = v
	}
	return tftypes.NewValue(objType, values)
}

func policy(schedule string, hour, weekday, retention int64) map[string]tftypes.Value {
	out := map[string]tftypes.Value{
		"schedule":       tftypes.NewValue(tftypes.String, schedule),
		"retention_days": tftypes.NewValue(tftypes.Number, retention),
	}
	if hour >= 0 {
		out["hour"] = tftypes.NewValue(tftypes.Number, hour)
	}
	if weekday >= 0 {
		out["weekday"] = tftypes.NewValue(tftypes.Number, weekday)
	}
	return out
}

// Adding a block sends the whole policy in one PATCH, hour 0 included.
func TestBackupPolicyAddSendsTheWholePolicy(t *testing.T) {
	api := &instanceUpdateAPI{t: t, name: "web", description: "front"}
	_, s := instanceResourceFor(t, nil)
	m, diags := updateInstance(t, api, webInstance(t, s, nil),
		webInstance(t, s, map[string]tftypes.Value{
			"backup_policy": backupBlock(t, s, policy("weekly", 0, 3, 14)),
		}))
	if diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	sent := api.sent()
	if len(sent) != 1 {
		t.Fatalf("PATCH bodies = %v, want one", sent)
	}
	want := map[string]any{"backup_schedule": "weekly", "backup_hour": float64(0), "backup_weekday": float64(3), "backup_retention_days": float64(14)}
	if len(sent[0]) != len(want) {
		t.Errorf("PATCH = %v, want exactly %v", sent[0], want)
	}
	for k, v := range want {
		if sent[0][k] != v {
			t.Errorf("%s = %v, want %v", k, sent[0][k], v)
		}
	}
	if m.BackupPolicy.IsNull() {
		t.Fatal("state has no backup_policy block after the PATCH")
	}
}

// Removing the block turns scheduling off and sends nothing else.
func TestBackupPolicyRemovalSendsScheduleNone(t *testing.T) {
	api := &instanceUpdateAPI{t: t, name: "web", description: "front",
		backup: map[string]any{"backup_schedule": "daily", "backup_hour": float64(2), "backup_retention_days": float64(7)}}
	_, s := instanceResourceFor(t, nil)
	prior := webInstance(t, s, map[string]tftypes.Value{"backup_policy": backupBlock(t, s, policy("daily", -1, -1, 7))})
	m, diags := updateInstance(t, api, prior, webInstance(t, s, nil))
	if diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	sent := api.sent()
	if len(sent) != 1 || len(sent[0]) != 1 || sent[0]["backup_schedule"] != "none" {
		t.Fatalf("PATCH bodies = %v, want one {backup_schedule: none}", sent)
	}
	if !m.BackupPolicy.IsNull() {
		t.Errorf("backup_policy = %v, want null after removal", m.BackupPolicy)
	}
}

// An unchanged block sends no PATCH: the policy is not rewritten on every apply.
func TestBackupPolicyUnchangedSendsNoPatch(t *testing.T) {
	api := &instanceUpdateAPI{t: t, name: "web", description: "front"}
	_, s := instanceResourceFor(t, nil)
	block := webInstance(t, s, map[string]tftypes.Value{"backup_policy": backupBlock(t, s, policy("daily", 2, -1, 7))})
	if _, diags := updateInstance(t, api, block, block); diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	if sent := api.sent(); len(sent) != 0 {
		t.Fatalf("PATCH sent for an unchanged policy: %v", sent)
	}
}

// The platform reports "none" for an instance without a policy: the state gets
// no block, which matches a configuration without one.
func TestBackupPolicyReadMapsNoneToANullBlock(t *testing.T) {
	m := instanceModel{}
	applyInstance(&m, &nemau.Instance{ID: "i-1", Name: "web", BackupSchedule: nemau.BackupNone})
	if !m.BackupPolicy.IsNull() {
		t.Errorf("backup_policy = %v, want null", m.BackupPolicy)
	}
}

// A weekly policy read back keeps its weekday and hour; a missing hour falls
// back to the platform default, 2.
func TestBackupPolicyReadMapsWeeklyFields(t *testing.T) {
	hour, weekday, retention := 5, 6, 35
	m := instanceModel{}
	applyInstance(&m, &nemau.Instance{ID: "i-1", Name: "web", BackupSchedule: nemau.BackupWeekly,
		BackupHour: &hour, BackupWeekday: &weekday, BackupRetentionDays: &retention})
	attrs := m.BackupPolicy.Attributes()
	if attrs["hour"].String() != "5" || attrs["weekday"].String() != "6" || attrs["retention_days"].String() != "35" {
		t.Errorf("block = %v", attrs)
	}

	m = instanceModel{}
	applyInstance(&m, &nemau.Instance{ID: "i-1", Name: "web", BackupSchedule: nemau.BackupDaily, BackupRetentionDays: &retention})
	if got := m.BackupPolicy.Attributes()["hour"].String(); got != "2" {
		t.Errorf("hour = %s, want the default 2", got)
	}
}

// Out-of-range values are refused at plan time, before any PATCH.
func TestBackupPolicyConfigRefusesOutOfRangeValues(t *testing.T) {
	cases := map[string]map[string]tftypes.Value{
		"schedule none":   policy("none", 2, 0, 7),
		"schedule hourly": policy("hourly", 2, 0, 7),
		"hour 24":         policy("daily", 24, 0, 7),
		"weekday 7":       policy("weekly", 2, 7, 7),
		"retention 0":     policy("daily", 2, 0, 0),
		"retention 36":    policy("daily", 2, 0, 36),
	}
	for name, fields := range cases {
		t.Run(name, func(t *testing.T) {
			r, s := instanceResourceFor(t, nil)
			raw := webInstance(t, s, map[string]tftypes.Value{"backup_policy": backupBlock(t, s, fields)})
			req := resource.ValidateConfigRequest{Config: tfsdk.Config{Schema: s, Raw: raw}}
			var resp resource.ValidateConfigResponse
			r.(resource.ResourceWithValidateConfig).ValidateConfig(context.Background(), req, &resp)
			if !resp.Diagnostics.HasError() {
				t.Errorf("no error for %s", name)
			}
		})
	}
}

func TestBackupPolicyConfigAcceptsTheBounds(t *testing.T) {
	r, s := instanceResourceFor(t, nil)
	for _, fields := range []map[string]tftypes.Value{
		policy("daily", 0, -1, 1),
		policy("weekly", 23, 0, 35),
		policy("weekly", 2, 6, 7),
	} {
		raw := webInstance(t, s, map[string]tftypes.Value{"backup_policy": backupBlock(t, s, fields)})
		req := resource.ValidateConfigRequest{Config: tfsdk.Config{Schema: s, Raw: raw}}
		var resp resource.ValidateConfigResponse
		r.(resource.ResourceWithValidateConfig).ValidateConfig(context.Background(), req, &resp)
		if resp.Diagnostics.HasError() {
			t.Errorf("bounds refused: %v", resp.Diagnostics)
		}
	}
}
