package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// createInstance runs Create against the stub. saved reports whether the
// response carries a state: Terraform records the resource only then.
func createInstance(t *testing.T, api *instanceUpdateAPI, planned tftypes.Value) (m instanceModel, saved bool, diags string) {
	t.Helper()
	ctx := context.Background()
	r, s := instanceResourceFor(t, api.client())
	resp := resource.CreateResponse{
		State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil)},
	}
	r.Create(ctx, resource.CreateRequest{Plan: tfsdk.Plan{Schema: s, Raw: planned}}, &resp)
	messages := make([]string, 0, len(resp.Diagnostics))
	for _, d := range resp.Diagnostics {
		messages = append(messages, d.Summary()+": "+d.Detail())
	}
	diags = strings.Join(messages, "\n")
	if resp.State.Raw.IsNull() {
		return m, false, diags
	}
	if d := resp.State.Get(ctx, &m); d.HasError() {
		t.Fatal(d)
	}
	return m, true, diags
}

// A create with a backup policy makes the instance, then sends the policy in
// one PATCH, and the state holds both.
func TestCreateWithBackupPolicySendsThePolicy(t *testing.T) {
	api := &instanceUpdateAPI{t: t, name: "web", description: "front"}
	_, s := instanceResourceFor(t, nil)
	m, saved, diags := createInstance(t, api, webInstance(t, s, map[string]tftypes.Value{
		"backup_policy": backupBlock(t, s, policy("daily", 2, -1, 7)),
	}))
	if diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	if !saved || m.ID.ValueString() != "i-1" {
		t.Fatalf("state saved = %v, id = %v", saved, m.ID)
	}
	if sent := api.sent(); len(sent) != 1 || sent[0]["backup_schedule"] != "daily" {
		t.Fatalf("PATCH bodies = %v, want one daily policy", sent)
	}
	if m.BackupPolicy.IsNull() {
		t.Error("state has no backup_policy after a successful PATCH")
	}
}

// The PATCH fails after the instance exists. The state must still be saved,
// without the policy, or Terraform loses the instance and a retry creates a
// second one.
func TestCreateBackupPatchFailureSavesTheInstance(t *testing.T) {
	api := &instanceUpdateAPI{t: t, name: "web", description: "front", failing: map[string]bool{"t-1": true}}
	_, s := instanceResourceFor(t, nil)
	m, saved, diags := createInstance(t, api, webInstance(t, s, map[string]tftypes.Value{
		"backup_policy": backupBlock(t, s, policy("daily", 2, -1, 7)),
	}))
	if diags == "" {
		t.Fatal("no diagnostic for the failed PATCH")
	}
	if !saved {
		t.Fatalf("state not saved after the PATCH failed; the instance would be lost: %s", diags)
	}
	if m.ID.ValueString() != "i-1" {
		t.Errorf("id = %v, want i-1", m.ID)
	}
	if !m.BackupPolicy.IsNull() {
		t.Errorf("backup_policy = %v, want null after the PATCH failed", m.BackupPolicy)
	}
}

// The stop fails after the instance exists: the state is saved with the
// platform's status, and the policy is never sent.
func TestCreateStopFailureSavesTheInstance(t *testing.T) {
	api := &instanceUpdateAPI{t: t, name: "web", description: "front", failing: map[string]bool{"t-stop": true}}
	_, s := instanceResourceFor(t, nil)
	m, saved, diags := createInstance(t, api, webInstance(t, s, map[string]tftypes.Value{
		"started":       tftypes.NewValue(tftypes.Bool, false),
		"backup_policy": backupBlock(t, s, policy("daily", 2, -1, 7)),
	}))
	if diags == "" {
		t.Fatal("no diagnostic for the failed stop")
	}
	if !saved {
		t.Fatalf("state not saved after the stop failed; the instance would be lost: %s", diags)
	}
	if m.ID.ValueString() != "i-1" || !m.Started.ValueBool() {
		t.Errorf("state = id %v, started %v; want i-1 running as the platform reports", m.ID, m.Started)
	}
	if sent := api.sent(); len(sent) != 0 {
		t.Errorf("PATCH sent after the stop failed: %v", sent)
	}
}
