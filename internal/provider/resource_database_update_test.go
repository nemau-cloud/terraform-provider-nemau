package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

const dbPath = "/api/v1/databases/db-1"

// subnetFixture is a subnet id in the form the SDK checks. It is not real.
const subnetFixture = "5c1f1e0a-9d2b-4e3a-8f7c-1a2b3c4d5e6f"

// dbStub stands in for one database and its task. Its GET answers the stored
// view; a resize with apply_at maintenance_window is recorded as pending and not
// applied, as the platform does; the rotation answers a fixture password.
type dbStub struct {
	t *testing.T

	mu         sync.Mutex
	db         nemau.Database
	list       []nemau.Database // answer of the listing
	getStatus  int              // when set, a read of the database fails with it
	source     nemau.Database   // the restore source, read at db-src
	taskStatus string           // terminal status of every task; "completed" by default
	requests   []string
	bodies     map[string]map[string]any
}

func (d *dbStub) client() *nemau.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		key := r.Method + " " + r.URL.Path
		d.requests = append(d.requests, key)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/t-1":
			status := d.taskStatus
			if status == "" {
				status = "completed"
			}
			_, _ = w.Write([]byte(`{"id":"t-1","status":"` + status + `","progress":100,"resourceId":"db-1"}`))
		case r.Method == http.MethodGet && r.URL.Path == dbPath:
			if d.getStatus != 0 {
				w.WriteHeader(d.getStatus)
				_, _ = w.Write([]byte(`{"error":"unavailable"}`))
				return
			}
			out, _ := json.Marshal(d.db)
			_, _ = w.Write(out)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/databases/db-src":
			out, _ := json.Marshal(d.source)
			_, _ = w.Write(out)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/databases/db-src/restore":
			d.record(r, key)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task_id":"t-1","username":"appadmin","password":"` + fixtureDBPassword + `"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/databases":
			out, _ := json.Marshal(map[string]any{"databases": d.list})
			_, _ = w.Write(out)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/databases":
			d.record(r, key)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task_id":"t-1","username":"appadmin","password":"` + fixtureDBPassword + `"}`))
		case r.Method == http.MethodPost && r.URL.Path == dbPath+"/resize":
			body := d.record(r, key)
			if body["apply_at"] == "maintenance_window" {
				d.db.PendingResize = &nemau.PendingResize{Flavor: str2(body["flavor"]), StorageGB: int(num(body["storage_gb"]))}
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task_id":"t-1","status":"pending"}`))
		case r.Method == http.MethodPost && r.URL.Path == dbPath+"/rotate-password":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task_id":"t-1","database_id":"db-1","username":"appadmin","password":"` + fixtureRotated + `"}`))
		case r.Method == http.MethodPatch && r.URL.Path == dbPath:
			d.record(r, key)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task_id":"t-1","status":"pending"}`))
		default:
			d.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	d.t.Cleanup(srv.Close)
	c, err := nemau.NewClient(
		nemau.WithBaseURL(srv.URL),
		nemau.WithCredential(nemau.AccessKey{ID: "fake", Secret: "fake"}),
		nemau.WithRetry(nemau.RetryPolicy{MaxAttempts: 1}),
		nemau.WithPollInterval(time.Millisecond),
	)
	if err != nil {
		d.t.Fatalf("NewClient: %v", err)
	}
	return c
}

// record decodes a request body and keeps it under its request key.
func (d *dbStub) record(r *http.Request, key string) map[string]any {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		d.t.Errorf("%s body is not JSON: %v", key, err)
	}
	if d.bodies == nil {
		d.bodies = map[string]map[string]any{}
	}
	d.bodies[key] = body
	return body
}

func (d *dbStub) sent(key string) (map[string]any, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	body, ok := d.bodies[key]
	return body, ok
}

func str2(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

// fixtureRotated stands in for a rotated password. It is never real.
const fixtureRotated = "FixtureRotatedNotReal0123456789"

// dbModelOf is the model of database db-1 as the configuration and the state
// hold it, with the changes applied.
func dbModelOf(t *testing.T, mutate func(*databaseModel)) databaseModel {
	t.Helper()
	m := databaseModel{
		ID:                      types.StringValue("db-1"),
		Name:                    types.StringValue("orders"),
		Description:             types.StringNull(),
		Version:                 types.StringValue("16"),
		Flavor:                  types.StringValue("db-small"),
		StorageGB:               types.Int64Value(20),
		SubnetID:                types.StringValue(subnetFixture),
		SecurityGroupIDs:        types.ListNull(types.StringType),
		AdminUsername:           types.StringValue("appadmin"),
		AdminPassword:           types.StringValue(fixtureDBPassword),
		PasswordRotationTrigger: types.StringNull(),
		BackupRetentionDays:     types.Int64Value(7),
		MaintenanceWindow:       types.ObjectNull(windowAttrTypes),
		ResizeApplyAt:           types.StringValue("now"),
		PurgeBackupsOnDestroy:   types.BoolNull(),
		RestoreFrom:             types.ObjectNull(restoreAttrTypes()),
		Status:                  types.StringValue("active"),
		Address:                 types.StringValue("10.0.0.5"),
		Port:                    types.Int64Value(5432),
	}
	if mutate != nil {
		mutate(&m)
	}
	return m
}

// storedDB is the database as the platform holds it before a test changes it.
func storedDB() nemau.Database {
	return nemau.Database{
		ID: "db-1", Name: "orders", Version: "16", Flavor: "db-small", StorageGB: 20,
		SubnetID: subnetFixture, AdminUsername: "appadmin", Port: 5432, Address: "10.0.0.5",
		Status: "active", BackupRetentionDays: 7,
	}
}

// runDBUpdate applies an update from prior to planned and returns the
// resulting state and the diagnostics as text.
func runDBUpdate(t *testing.T, stub *dbStub, prior, planned databaseModel) (databaseModel, string) {
	t.Helper()
	ctx := context.Background()
	r := NewDatabaseResource().(*databaseResource)
	r.client = stub.client()

	var s resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &s)
	state := tfsdk.State{Schema: s.Schema}
	if d := state.Set(ctx, &prior); d.HasError() {
		t.Fatal(d)
	}
	plan := tfsdk.Plan{Schema: s.Schema}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatal(d)
	}
	resp := resource.UpdateResponse{State: state}
	r.Update(ctx, resource.UpdateRequest{
		Plan:   plan,
		State:  state,
		Config: tfsdk.Config{Schema: s.Schema, Raw: plan.Raw},
	}, &resp)
	var got databaseModel
	if d := resp.State.Get(ctx, &got); d.HasError() {
		t.Fatal(d)
	}
	return got, diagText(resp.Diagnostics)
}

// A create that answers its password, then fails to read the database back:
// the id and the password are in state, so the database is not orphaned.
func TestDatabaseCreateKeepsTheIDWhenTheReadFails(t *testing.T) {
	stub := &dbStub{t: t, db: storedDB(), getStatus: http.StatusInternalServerError}
	ctx := context.Background()
	r := NewDatabaseResource().(*databaseResource)
	r.client = stub.client()

	var s resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &s)
	planned := dbModelOf(t, func(m *databaseModel) {
		m.ID = types.StringUnknown()
		m.AdminPassword = types.StringUnknown()
		m.Status = types.StringUnknown()
		m.Address = types.StringUnknown()
		m.Port = types.Int64Unknown()
		m.BackupRetentionDays = types.Int64Unknown()
		m.ResizeApplyAt = types.StringValue("now")
	})
	plan := tfsdk.Plan{Schema: s.Schema}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatal(d)
	}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s.Schema, Raw: tftypes.NewValue(s.Schema.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan, Config: tfsdk.Config{Schema: s.Schema, Raw: plan.Raw}}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("the failed read was not reported")
	}
	var got databaseModel
	if d := resp.State.Get(ctx, &got); d.HasError() {
		t.Fatal(d)
	}
	if got.ID.ValueString() != "db-1" || got.AdminPassword.ValueString() != fixtureDBPassword {
		t.Errorf("state id %v, password %v; the database must not be orphaned", got.ID, got.AdminPassword)
	}
}

// A database the platform reports as deleted is removed from state, so the next
// plan creates it again.
func TestDatabaseReadRemovesADeletedDatabase(t *testing.T) {
	ctx := context.Background()
	deleted := storedDB()
	deleted.Status = statusDeleted
	stub := &dbStub{t: t, db: deleted}
	r := NewDatabaseResource().(*databaseResource)
	r.client = stub.client()

	var s resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &s)
	state := tfsdk.State{Schema: s.Schema}
	prior := dbModelOf(t, nil)
	if d := state.Set(ctx, &prior); d.HasError() {
		t.Fatal(d)
	}
	resp := resource.ReadResponse{State: state}
	r.Read(ctx, resource.ReadRequest{State: state}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("diagnostics: %s", diagText(resp.Diagnostics))
	}
	if !resp.State.Raw.IsNull() {
		t.Errorf("a deleted database is still in state: %v", resp.State.Raw)
	}
}

// Matching by name skips a deleted database that keeps the name.
func TestDatabaseNamedSkipsDeletedDatabases(t *testing.T) {
	deleted := storedDB()
	deleted.Status = statusDeleted
	stub := &dbStub{t: t, list: []nemau.Database{deleted, storedDB()}}
	matches, err := databasesNamed(context.Background(), stub.client(), "orders")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].Status == statusDeleted {
		t.Errorf("matches = %+v, want the one active database", matches)
	}
}

// A deferred resize is accepted but not applied: the state keeps the planned
// values, so the apply's result is the plan.
func TestDatabaseDeferredResizeKeepsThePlannedValues(t *testing.T) {
	stub := &dbStub{t: t, db: storedDB()}
	prior := dbModelOf(t, nil)
	planned := dbModelOf(t, func(m *databaseModel) {
		m.Flavor = types.StringValue("db-large")
		m.StorageGB = types.Int64Value(50)
		m.ResizeApplyAt = types.StringValue("maintenance_window")
	})
	got, diags := runDBUpdate(t, stub, prior, planned)
	if diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	if got.Flavor.ValueString() != "db-large" || got.StorageGB.ValueInt64() != 50 {
		t.Errorf("state = flavor %v, storage %v; want the planned db-large and 50", got.Flavor, got.StorageGB)
	}
}

func TestFillDatabaseKeepsPlannedValuesOnlyWhileTheResizeIsPending(t *testing.T) {
	ctx := context.Background()
	planned := func() databaseModel {
		return dbModelOf(t, func(m *databaseModel) {
			m.Flavor = types.StringValue("db-large")
			m.StorageGB = types.Int64Value(50)
		})
	}
	view := storedDB()

	pending := planned()
	view.PendingResize = &nemau.PendingResize{Flavor: "db-large", StorageGB: 50}
	if d := fillDatabase(ctx, &pending, &view); d.HasError() {
		t.Fatal(d)
	}
	if pending.Flavor.ValueString() != "db-large" || pending.StorageGB.ValueInt64() != 50 {
		t.Errorf("pending resize: flavor %v, storage %v; want the planned values", pending.Flavor, pending.StorageGB)
	}

	applied := planned()
	done := storedDB()
	done.Flavor, done.StorageGB = "db-large", 50
	if d := fillDatabase(ctx, &applied, &done); d.HasError() {
		t.Fatal(d)
	}
	if applied.Flavor.ValueString() != "db-large" || applied.StorageGB.ValueInt64() != 50 {
		t.Errorf("applied resize: flavor %v, storage %v", applied.Flavor, applied.StorageGB)
	}

	// A pending resize to other values is not the plan: the current values are read.
	other := planned()
	view.PendingResize = &nemau.PendingResize{Flavor: "db-xl", StorageGB: 80}
	if d := fillDatabase(ctx, &other, &view); d.HasError() {
		t.Fatal(d)
	}
	if other.Flavor.ValueString() != "db-small" || other.StorageGB.ValueInt64() != 20 {
		t.Errorf("unrelated pending resize: flavor %v, storage %v; want the current values", other.Flavor, other.StorageGB)
	}
}

// The new password is kept in state before the rotation task is waited for: a
// failed task must not lose the only copy of it.
func TestDatabaseRotationKeepsThePasswordWhenTheTaskFails(t *testing.T) {
	stub := &dbStub{t: t, db: storedDB(), taskStatus: "failed"}
	prior := dbModelOf(t, func(m *databaseModel) { m.PasswordRotationTrigger = types.StringValue("v1") })
	planned := dbModelOf(t, func(m *databaseModel) {
		m.PasswordRotationTrigger = types.StringValue("v2")
		m.AdminPassword = types.StringUnknown()
	})
	got, diags := runDBUpdate(t, stub, prior, planned)
	if !strings.Contains(diags, "Rotating the administrator password failed") {
		t.Fatalf("diagnostics = %q, want the failed rotation", diags)
	}
	if got.AdminPassword.ValueString() != fixtureRotated {
		t.Errorf("state password = %q; the rotated one must be kept", got.AdminPassword.ValueString())
	}
	if got.PasswordRotationTrigger.ValueString() != "v2" {
		t.Errorf("trigger = %v, want v2 so the rotation is not run twice", got.PasswordRotationTrigger)
	}
}

// restoreModelOf is the plan of a restore of db-src at a past time, with the
// given changes applied.
func restoreModelOf(t *testing.T, mutate func(*databaseModel)) databaseModel {
	t.Helper()
	ctx := context.Background()
	rf, d := types.ObjectValueFrom(ctx, restoreAttrTypes(), restoreFromModel{
		SourceDatabaseID: types.StringValue("db-src"), TargetTime: types.StringValue("2026-10-20T09:30:00Z"),
	})
	if d.HasError() {
		t.Fatal(d)
	}
	return dbModelOf(t, func(m *databaseModel) {
		m.ID = types.StringUnknown()
		m.AdminPassword = types.StringUnknown()
		m.Status = types.StringUnknown()
		m.Address = types.StringUnknown()
		m.Port = types.Int64Unknown()
		m.Version = types.StringUnknown()
		m.BackupRetentionDays = types.Int64Unknown()
		m.ResizeApplyAt = types.StringValue("now")
		m.RestoreFrom = rf
		if mutate != nil {
			mutate(m)
		}
	})
}

// createDatabase runs a create of planned and returns the state and the diagnostics.
func createDatabase(t *testing.T, stub *dbStub, planned databaseModel) (databaseModel, string) {
	t.Helper()
	ctx := context.Background()
	r := NewDatabaseResource().(*databaseResource)
	r.client = stub.client()
	var s resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &s)
	plan := tfsdk.Plan{Schema: s.Schema}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatal(d)
	}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s.Schema, Raw: tftypes.NewValue(s.Schema.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan, Config: tfsdk.Config{Schema: s.Schema, Raw: plan.Raw}}, &resp)
	var got databaseModel
	if !resp.State.Raw.IsNull() {
		if d := resp.State.Get(ctx, &got); d.HasError() {
			t.Fatal(d)
		}
	}
	return got, diagText(resp.Diagnostics)
}

// planDatabase runs ModifyPlan of a database from prior to planned, with the
// configuration planned was made from. See planDatabaseWithConfig.
func planDatabase(t *testing.T, stub *dbStub, prior *databaseModel, planned databaseModel) (databaseModel, string) {
	t.Helper()
	return planDatabaseWithConfig(t, stub, prior, planned, planned)
}

// planDatabaseWithConfig runs ModifyPlan of a database from prior to planned
// under the given configuration and returns the planned state and the
// diagnostics.
func planDatabaseWithConfig(t *testing.T, stub *dbStub, prior *databaseModel, planned, cfg databaseModel) (databaseModel, string) {
	t.Helper()
	ctx := context.Background()
	r := NewDatabaseResource().(*databaseResource)
	r.client = stub.client()
	var s resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &s)
	state := tfsdk.State{Schema: s.Schema, Raw: tftypes.NewValue(s.Schema.Type().TerraformType(ctx), nil)}
	if prior != nil {
		if d := state.Set(ctx, prior); d.HasError() {
			t.Fatal(d)
		}
	}
	plan := tfsdk.Plan{Schema: s.Schema}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatal(d)
	}
	configPlan := tfsdk.Plan{Schema: s.Schema}
	if d := configPlan.Set(ctx, &cfg); d.HasError() {
		t.Fatal(d)
	}
	resp := resource.ModifyPlanResponse{Plan: plan}
	r.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: plan, State: state, Config: tfsdk.Config{Schema: s.Schema, Raw: configPlan.Raw}}, &resp)
	var got databaseModel
	if d := resp.Plan.Get(ctx, &got); d.HasError() {
		t.Fatal(d)
	}
	return got, diagText(resp.Diagnostics)
}

// A restore takes the source's settings; the retention and window the
// configuration sets are applied after it, with one update.
func TestDatabaseRestoreAppliesRetentionAndWindowAfterTheRestore(t *testing.T) {
	stub := &dbStub{t: t, db: storedDB()}
	planned := restoreModelOf(t, func(m *databaseModel) {
		m.BackupRetentionDays = types.Int64Value(14)
		m.MaintenanceWindow = windowObject(t, 0, 3)
	})
	_, diags := createDatabase(t, stub, planned)
	if diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	if _, ok := stub.sent("POST /api/v1/databases/db-src/restore"); !ok {
		t.Fatal("the restore was not sent")
	}
	body, ok := stub.sent("PATCH " + dbPath)
	if !ok {
		t.Fatal("the restored database was not updated")
	}
	window, _ := body["maintenance_window"].(map[string]any)
	if body["backup_retention_days"] != float64(14) || window["weekday"] != float64(0) || window["hour"] != float64(3) {
		t.Errorf("PATCH body = %v, want retention 14 and window Sunday 03:00", body)
	}
}

// A restore whose settings already match the restored database is not updated.
func TestDatabaseRestoreWithMatchingSettingsIsNotUpdated(t *testing.T) {
	stub := &dbStub{t: t, db: storedDB()}
	planned := restoreModelOf(t, func(m *databaseModel) { m.BackupRetentionDays = types.Int64Value(7) })
	if _, diags := createDatabase(t, stub, planned); diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	if _, ok := stub.sent("PATCH " + dbPath); ok {
		t.Error("the restored database was updated although its settings match")
	}
}

// A restore whose version is not the source's is refused at plan time.
func TestDatabaseRestorePlanRefusesAnotherVersion(t *testing.T) {
	source := storedDB()
	source.ID, source.Version = "db-src", "16"
	stub := &dbStub{t: t, source: source}

	_, diags := planDatabase(t, stub, nil, restoreModelOf(t, func(m *databaseModel) { m.Version = types.StringValue("15") }))
	if !strings.Contains(diags, "Restore keeps the source's version") {
		t.Fatalf("diagnostics = %q, want the refusal", diags)
	}
	if _, diags := planDatabase(t, stub, nil, restoreModelOf(t, func(m *databaseModel) { m.Version = types.StringValue("16") })); diags != "" {
		t.Errorf("the source's version was refused: %s", diags)
	}
}

// A plan that moves an existing database into restore_from replaces it, and the
// restore it creates is refused at plan time when its version is not the source's.
func TestDatabaseReplacementIntoARestoreIsCheckedAtPlanTime(t *testing.T) {
	source := storedDB()
	source.ID, source.Version = "db-src", "16"
	stub := &dbStub{t: t, source: source, db: storedDB()}
	prior := dbModelOf(t, nil)

	_, diags := planDatabase(t, stub, &prior, restoreModelOf(t, func(m *databaseModel) { m.Version = types.StringValue("15") }))
	if !strings.Contains(diags, "Restore keeps the source's version") {
		t.Fatalf("diagnostics = %q, want the version refusal for a replacement", diags)
	}
	if _, diags := planDatabase(t, stub, &prior, restoreModelOf(t, func(m *databaseModel) { m.Version = types.StringValue("16") })); diags != "" {
		t.Errorf("the source's version was refused for a replacement: %s", diags)
	}
}

// A replacement into a restore whose configuration omits version is not
// refused: the plan carries the state's version (UseStateForUnknown), which says
// nothing about the source, and the restore sets the version.
func TestDatabaseReplacementIntoARestoreWithOmittedVersionIsNotRefused(t *testing.T) {
	source := storedDB()
	source.ID, source.Version = "db-src", "16"
	stub := &dbStub{t: t, source: source, db: storedDB()}
	prior := dbModelOf(t, func(m *databaseModel) { m.Version = types.StringValue("15") })
	planned := restoreModelOf(t, func(m *databaseModel) { m.Version = types.StringValue("15") })
	cfg := planned
	cfg.Version = types.StringNull()

	if _, diags := planDatabaseWithConfig(t, stub, &prior, planned, cfg); diags != "" {
		t.Errorf("a restore with version omitted was refused: %s", diags)
	}
}

// A replacement into a restore whose configuration sets a version other than
// the source's is refused, even though the plan carries the same value.
func TestDatabaseReplacementIntoARestoreWithAConfiguredVersionIsRefused(t *testing.T) {
	source := storedDB()
	source.ID, source.Version = "db-src", "16"
	stub := &dbStub{t: t, source: source, db: storedDB()}
	prior := dbModelOf(t, func(m *databaseModel) { m.Version = types.StringValue("15") })
	planned := restoreModelOf(t, func(m *databaseModel) { m.Version = types.StringValue("15") })

	if _, diags := planDatabaseWithConfig(t, stub, &prior, planned, planned); !strings.Contains(diags, "Restore keeps the source's version") {
		t.Fatalf("diagnostics = %q, want the refusal for a configured version", diags)
	}
}

// A plan that replaces the database is not refused for a shrink or a change of
// security groups, which the new database does not carry over; its password is
// new.
func TestDatabasePlanReplacementSkipsImmutabilityErrors(t *testing.T) {
	stub := &dbStub{t: t, db: storedDB()}
	prior := dbModelOf(t, nil)

	shrunk := dbModelOf(t, func(m *databaseModel) { m.StorageGB = types.Int64Value(10) })
	if _, diags := planDatabase(t, stub, &prior, shrunk); !strings.Contains(diags, "Storage cannot shrink") {
		t.Fatalf("an in-place shrink was not refused: %q", diags)
	}

	replaced := dbModelOf(t, func(m *databaseModel) {
		m.StorageGB = types.Int64Value(10)
		m.SubnetID = types.StringValue("6d2e2f1b-0e3c-4f4b-9a8d-2b3c4d5e6f70")
		m.SecurityGroupIDs = strListOf("7e3f3a2c-1f4d-4a5b-8b9e-3c4d5e6f7081")
	})
	got, diags := planDatabase(t, stub, &prior, replaced)
	if diags != "" {
		t.Errorf("a replacement was refused: %s", diags)
	}
	if !got.AdminPassword.IsUnknown() {
		t.Errorf("the password of a replaced database = %v, want unknown: the new database issues its own", got.AdminPassword)
	}
}

func windowObject(t *testing.T, weekday, hour int64) types.Object {
	t.Helper()
	obj, d := types.ObjectValueFrom(context.Background(), windowAttrTypes, windowModel{
		Weekday: types.Int64Value(weekday), Hour: types.Int64Value(hour),
	})
	if d.HasError() {
		t.Fatal(d)
	}
	return obj
}

func strListOf(values ...string) types.List {
	return strList(values)
}

// Removing the maintenance_window block plans its removal, and the update sends
// an explicit null, which the platform reads as a clear.
func TestDatabaseRemovingTheWindowSendsAClear(t *testing.T) {
	stub := &dbStub{t: t, db: storedDB()}
	stub.db.MaintenanceWindow = &nemau.DatabaseMaintenanceWindow{Weekday: 0, Hour: 3}
	prior := dbModelOf(t, func(m *databaseModel) { m.MaintenanceWindow = windowObject(t, 0, 3) })
	planned := dbModelOf(t, func(m *databaseModel) { m.MaintenanceWindow = types.ObjectNull(windowAttrTypes) })

	_, diags := runDBUpdate(t, stub, prior, planned)
	if diags != "" {
		t.Fatalf("update diagnostics: %s", diags)
	}
	body, ok := stub.sent("PATCH " + dbPath)
	if !ok {
		t.Fatal("no PATCH sent")
	}
	if v, present := body["maintenance_window"]; !present || v != nil {
		t.Errorf("maintenance_window = %v (present %t), want an explicit null", v, present)
	}
}

// A removed block plans a null window, so the state does not keep the old one.
func TestDatabaseModifyPlanNullsARemovedWindow(t *testing.T) {
	stub := &dbStub{t: t, db: storedDB()}
	prior := dbModelOf(t, func(m *databaseModel) { m.MaintenanceWindow = windowObject(t, 0, 3) })
	// The plan carries the state's window, as UseStateForUnknown gives it; the
	// configuration omits the block.
	planned := dbModelOf(t, func(m *databaseModel) { m.MaintenanceWindow = windowObject(t, 0, 3) })
	cfg := dbModelOf(t, func(m *databaseModel) { m.MaintenanceWindow = types.ObjectNull(windowAttrTypes) })

	got, diags := planDatabaseWithConfig(t, stub, &prior, planned, cfg)
	if diags != "" {
		t.Fatalf("plan diagnostics: %s", diags)
	}
	if !got.MaintenanceWindow.IsNull() {
		t.Errorf("planned window = %v, want null", got.MaintenanceWindow)
	}
}

// A new database whose configuration omits the window has none: a window the
// platform set at create is cleared.
func TestDatabaseCreateClearsAPlatformWindowOmittedFromConfig(t *testing.T) {
	stub := &dbStub{t: t, db: storedDB()}
	stub.db.MaintenanceWindow = &nemau.DatabaseMaintenanceWindow{Weekday: 6, Hour: 2}
	planned := dbModelOf(t, func(m *databaseModel) { m.MaintenanceWindow = types.ObjectNull(windowAttrTypes) })

	_, _ = createDatabase(t, stub, planned)
	body, ok := stub.sent("PATCH " + dbPath)
	if !ok {
		t.Fatal("no clear sent after create")
	}
	if v, present := body["maintenance_window"]; !present || v != nil {
		t.Errorf("maintenance_window = %v (present %t), want an explicit null", v, present)
	}
}
