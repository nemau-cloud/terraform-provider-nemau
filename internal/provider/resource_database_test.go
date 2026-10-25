package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

// The password fixture is never a real credential.
const fixtureDBPassword = "FixturePasswordNotReal0123456789ab"

func TestFillDatabaseKeepsThePasswordAndTakesTheAPIValues(t *testing.T) {
	ctx := context.Background()
	m := databaseModel{
		ID:            types.StringValue("db-1"),
		AdminPassword: types.StringValue(fixtureDBPassword),
		Description:   types.StringNull(),
		ResizeApplyAt: types.StringUnknown(),
		RestoreFrom:   types.ObjectNull(restoreAttrTypes()),
	}
	db := &nemau.Database{
		ID: "db-1", Name: "orders-db", Version: "16", Flavor: "db-small", StorageGB: 20,
		SubnetID: "subnet-1", SecurityGroupIDs: []string{"sg-1"}, AdminUsername: "appadmin",
		Port: 5432, Address: "10.0.0.5", BackupRetentionDays: 14, Status: "READY",
		MaintenanceWindow: &nemau.DatabaseMaintenanceWindow{Weekday: 0, Hour: 3},
	}

	diags := fillDatabase(ctx, &m, db)
	if diags.HasError() {
		t.Fatalf("fill: %v", diags)
	}
	if m.AdminPassword.ValueString() != fixtureDBPassword {
		t.Error("the password was overwritten by a read: it must only come from create or rotation")
	}
	if !m.Description.IsNull() {
		t.Errorf("an empty description read back as %v; it must stay null", m.Description)
	}
	if m.ResizeApplyAt.ValueString() != nemau.ApplyNow {
		t.Errorf("resize_apply_at = %v, want the default now", m.ResizeApplyAt)
	}
	if m.Port.ValueInt64() != 5432 || m.Status.ValueString() != "READY" || m.Address.ValueString() != "10.0.0.5" {
		t.Errorf("computed values not filled: port=%v status=%v address=%v", m.Port, m.Status, m.Address)
	}
	w, d := windowFromObject(ctx, m.MaintenanceWindow)
	if d.HasError() || w == nil || w.Weekday != 0 || w.Hour != 3 {
		t.Errorf("maintenance window = %+v, %v", w, d)
	}
}

func TestOptionalStringsStayNullWhenUnsetAndEmpty(t *testing.T) {
	if !optionalString("", types.StringNull()).IsNull() {
		t.Error("empty value with a null prior must stay null")
	}
	if got := optionalString("x", types.StringNull()); got.ValueString() != "x" {
		t.Errorf("value = %v", got)
	}
	empty := types.ListNull(types.StringType)
	if !optionalStrList(nil, empty).IsNull() {
		t.Error("an empty security group list with a null prior must stay null")
	}
}

func TestMaintenanceWindowRoundTrip(t *testing.T) {
	ctx := context.Background()
	obj, d := objectFromWindow(ctx, &nemau.DatabaseMaintenanceWindow{Weekday: 6, Hour: 23})
	if d.HasError() {
		t.Fatal(d)
	}
	w, d := windowFromObject(ctx, obj)
	if d.HasError() || w == nil || w.Weekday != 6 || w.Hour != 23 {
		t.Errorf("round trip = %+v, %v", w, d)
	}
	nullObj, _ := objectFromWindow(ctx, nil)
	if !nullObj.IsNull() {
		t.Error("an absent window must be null")
	}
	if w, _ := windowFromObject(ctx, nullObj); w != nil {
		t.Errorf("null object read as %+v", w)
	}
}

func TestRestoreFromAbsentIsNotARestore(t *testing.T) {
	var d diag.Diagnostics
	_, restoring := restoreOf(context.Background(), types.ObjectNull(restoreAttrTypes()), &d)
	if restoring || d.HasError() {
		t.Errorf("restoring = %v, diags = %v", restoring, d)
	}
}

// restoreAttrTypes mirrors the restore_from block's attributes.
func restoreAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{"source_database_id": types.StringType, "target_time": types.StringType}
}
