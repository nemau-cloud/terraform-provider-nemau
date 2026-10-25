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
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

// containerUpdateAPI stands in for the container routes an update touches: the
// PATCH, its task, the listing and the live protection read.
type containerUpdateAPI struct {
	t *testing.T

	mu         sync.Mutex
	versioning string // "Enabled", "Suspended" or ""
	objectLock bool
	mode       string
	days       int

	// patchStatus overrides the PATCH answer (202 by default).
	patchStatus int
	// taskStatus is the terminal status of the update task ("completed" by default).
	taskStatus string
	// ignorePatch accepts the update without changing the store: a task that
	// completes but does not land.
	ignorePatch bool

	requests []string
	patches  []map[string]any
}

func (a *containerUpdateAPI) client() *nemau.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.requests = append(a.requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "PATCH /api/v1/storage/containers/photos":
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				a.t.Errorf("PATCH body %q: %v", raw, err)
			}
			a.patches = append(a.patches, body)
			if a.patchStatus != 0 {
				w.WriteHeader(a.patchStatus)
				_, _ = w.Write([]byte(`{"error":"retention can only be extended"}`))
				return
			}
			if !a.ignorePatch && (a.taskStatus == "" || a.taskStatus == "completed") {
				if v, ok := body["versioning"].(bool); ok {
					a.versioning = map[bool]string{true: "Enabled", false: "Suspended"}[v]
				}
				if d, ok := body["retention_days"].(float64); ok {
					a.days = int(d)
				}
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task_id":"t-1","status":"pending"}`))
		case "GET /api/v1/tasks/t-1":
			status := a.taskStatus
			if status == "" {
				status = "completed"
			}
			_, _ = w.Write([]byte(`{"id":"t-1","status":"` + status + `","progress":100}`))
		case "GET /api/v1/storage/containers":
			_, _ = w.Write([]byte(photosListing))
		case "GET /api/v1/storage/containers/photos/protection":
			out, _ := json.Marshal(map[string]any{
				"container": "photos",
				"protection": map[string]any{
					"versioning": a.versioning, "object_lock": a.objectLock,
					"retention_mode": a.mode, "retention_days": a.days,
				},
			})
			_, _ = w.Write(out)
		default:
			a.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	a.t.Cleanup(srv.Close)
	c, err := nemau.NewClient(
		nemau.WithBaseURL(srv.URL),
		nemau.WithCredential(nemau.AccessKey{ID: "fake", Secret: "fake"}),
		nemau.WithRetry(nemau.RetryPolicy{MaxAttempts: 1}),
		nemau.WithPollInterval(time.Millisecond),
	)
	if err != nil {
		a.t.Fatalf("NewClient: %v", err)
	}
	return c
}

// sent returns the PATCH bodies the stub received.
func (a *containerUpdateAPI) sent() []map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]map[string]any(nil), a.patches...)
}

// lockedPhotos is the state of a locked container with a 30-day retention.
func lockedPhotos(t *testing.T, s schema.Schema, overrides map[string]tftypes.Value) tftypes.Value {
	t.Helper()
	values := map[string]tftypes.Value{
		"versioning":     tftypes.NewValue(tftypes.Bool, true),
		"object_lock":    tftypes.NewValue(tftypes.Bool, true),
		"retention_mode": tftypes.NewValue(tftypes.String, "GOVERNANCE"),
		"retention_days": tftypes.NewValue(tftypes.Number, 30),
		"immutable":      tftypes.NewValue(tftypes.Bool, true),
		"bytes":          tftypes.NewValue(tftypes.Number, 12),
		"object_count":   tftypes.NewValue(tftypes.Number, 1),
		"s3_bucket":      tftypes.NewValue(tftypes.String, credFakeBucket),
	}
	for k, v := range overrides {
		values[k] = v
	}
	return containerState(t, s, values)
}

func lockedPhotosAPI(t *testing.T) *containerUpdateAPI {
	return &containerUpdateAPI{t: t, versioning: "Enabled", objectLock: true, mode: "GOVERNANCE", days: 30}
}

func updateContainer(t *testing.T, api *containerUpdateAPI, prior, planned tftypes.Value) (containerModel, string) {
	t.Helper()
	r, s := containerResourceFor(t, api.client())
	state := tfsdk.State{Schema: s, Raw: prior}
	resp := resource.UpdateResponse{State: state}
	r.Update(context.Background(), resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: s, Raw: planned},
		State: state,
	}, &resp)
	var m containerModel
	if diags := resp.State.Get(context.Background(), &m); diags.HasError() {
		t.Fatal(diags)
	}
	messages := make([]string, 0, len(resp.Diagnostics))
	for _, d := range resp.Diagnostics {
		messages = append(messages, d.Summary()+": "+d.Detail())
	}
	return m, strings.Join(messages, "\n")
}

// A longer retention reaches the API, alone, and the state reports the store.
func TestContainerUpdateRaisesTheRetention(t *testing.T) {
	api := lockedPhotosAPI(t)
	_, s := containerResourceFor(t, nil)
	m, diags := updateContainer(t, api,
		lockedPhotos(t, s, nil),
		lockedPhotos(t, s, map[string]tftypes.Value{"retention_days": tftypes.NewValue(tftypes.Number, 60)}))
	if diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	if sent := api.sent(); len(sent) != 1 || len(sent[0]) != 1 || sent[0]["retention_days"] != float64(60) {
		t.Fatalf("PATCH bodies = %v, want one {retention_days: 60}", sent)
	}
	if m.RetentionDays.ValueInt64() != 60 || !m.Immutable.ValueBool() {
		t.Errorf("state = retention %v, immutable %v", m.RetentionDays, m.Immutable)
	}
}

// Versioning toggles in place on a container without object lock.
func TestContainerUpdateTurnsVersioningOn(t *testing.T) {
	api := &containerUpdateAPI{t: t, versioning: "Suspended"}
	_, s := containerResourceFor(t, nil)
	unlocked := map[string]tftypes.Value{
		"versioning":     tftypes.NewValue(tftypes.Bool, false),
		"object_lock":    tftypes.NewValue(tftypes.Bool, false),
		"retention_mode": tftypes.NewValue(tftypes.String, ""),
		"retention_days": tftypes.NewValue(tftypes.Number, 0),
		"immutable":      tftypes.NewValue(tftypes.Bool, false),
	}
	planned := map[string]tftypes.Value{}
	for k, v := range unlocked {
		planned[k] = v
	}
	planned["versioning"] = tftypes.NewValue(tftypes.Bool, true)
	m, diags := updateContainer(t, api, lockedPhotos(t, s, unlocked), lockedPhotos(t, s, planned))
	if diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	if sent := api.sent(); len(sent) != 1 || len(sent[0]) != 1 || sent[0]["versioning"] != true {
		t.Fatalf("PATCH bodies = %v, want one {versioning: true}", sent)
	}
	if !m.Versioning.ValueBool() {
		t.Error("the state must record versioning on")
	}
}

// Nothing the API can change differs: no PATCH, and the computed attributes
// the plan left unknown are settled from the listing.
func TestContainerUpdateWithNothingToChangeSendsNoPatch(t *testing.T) {
	api := lockedPhotosAPI(t)
	_, s := containerResourceFor(t, nil)
	m, diags := updateContainer(t, api, lockedPhotos(t, s, nil), lockedPhotos(t, s, map[string]tftypes.Value{
		"bytes":        tftypes.NewValue(tftypes.Number, tftypes.UnknownValue),
		"object_count": tftypes.NewValue(tftypes.Number, tftypes.UnknownValue),
		"versioning":   tftypes.NewValue(tftypes.Bool, tftypes.UnknownValue),
	}))
	if diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	if sent := api.sent(); len(sent) != 0 {
		t.Fatalf("PATCH sent: %v", sent)
	}
	if m.Bytes.IsUnknown() || m.Count.IsUnknown() || m.Versioning.IsUnknown() {
		t.Errorf("unknown left in state: %+v", m)
	}
}

// A refused update is an error, and the state keeps what the store holds.
func TestContainerUpdateRefusedKeepsTheStoredRetention(t *testing.T) {
	api := lockedPhotosAPI(t)
	api.patchStatus = http.StatusConflict
	_, s := containerResourceFor(t, nil)
	m, diags := updateContainer(t, api,
		lockedPhotos(t, s, nil),
		lockedPhotos(t, s, map[string]tftypes.Value{"retention_days": tftypes.NewValue(tftypes.Number, 60)}))
	if !strings.Contains(diags, "Container update was refused") {
		t.Fatalf("diagnostics = %q", diags)
	}
	if m.RetentionDays.ValueInt64() != 30 {
		t.Errorf("state claims a retention of %v after a refusal", m.RetentionDays)
	}
}

// A failed task is an error, and the state keeps what the store holds.
func TestContainerUpdateFailedTaskKeepsTheStoredRetention(t *testing.T) {
	api := lockedPhotosAPI(t)
	api.taskStatus = "failed"
	_, s := containerResourceFor(t, nil)
	m, diags := updateContainer(t, api,
		lockedPhotos(t, s, nil),
		lockedPhotos(t, s, map[string]tftypes.Value{"retention_days": tftypes.NewValue(tftypes.Number, 60)}))
	if !strings.Contains(diags, "Container update failed") {
		t.Fatalf("diagnostics = %q", diags)
	}
	if m.RetentionDays.ValueInt64() != 30 {
		t.Errorf("state claims a retention of %v after a failed task", m.RetentionDays)
	}
}

// A task that completes without changing the store is reported as an error.
func TestContainerUpdateThatDidNotLandIsAnError(t *testing.T) {
	api := lockedPhotosAPI(t)
	api.ignorePatch = true
	_, s := containerResourceFor(t, nil)
	m, diags := updateContainer(t, api,
		lockedPhotos(t, s, nil),
		lockedPhotos(t, s, map[string]tftypes.Value{"retention_days": tftypes.NewValue(tftypes.Number, 60)}))
	if !strings.Contains(diags, "Container update not applied") || !strings.Contains(diags, "30 days") {
		t.Fatalf("diagnostics = %q", diags)
	}
	if m.RetentionDays.ValueInt64() != 30 {
		t.Errorf("state = %v, want the 30 days the store reports", m.RetentionDays)
	}
}

func planContainer(t *testing.T, prior, planned map[string]tftypes.Value) string {
	t.Helper()
	ctx := context.Background()
	r, s := containerResourceFor(t, nil)
	state := tftypes.NewValue(s.Type().TerraformType(ctx), nil)
	if prior != nil {
		state = lockedPhotos(t, s, prior)
	}
	req := resource.ModifyPlanRequest{
		Plan:  tfsdk.Plan{Schema: s, Raw: lockedPhotos(t, s, planned)},
		State: tfsdk.State{Schema: s, Raw: state},
	}
	resp := resource.ModifyPlanResponse{Plan: req.Plan}
	r.(resource.ResourceWithModifyPlan).ModifyPlan(ctx, req, &resp)
	messages := make([]string, 0, len(resp.Diagnostics))
	for _, d := range resp.Diagnostics {
		messages = append(messages, d.Summary()+": "+d.Detail())
	}
	return strings.Join(messages, "\n")
}

// The plan refuses a shorter WORM retention before anything runs.
func TestContainerPlanRefusesAShorterRetention(t *testing.T) {
	days := func(v any) map[string]tftypes.Value {
		return map[string]tftypes.Value{"retention_days": tftypes.NewValue(tftypes.Number, v)}
	}
	cases := []struct {
		label          string
		prior, planned map[string]tftypes.Value
		want           string // "" = allowed
	}{
		{"create", nil, days(7), ""},
		{"unchanged", days(30), days(30), ""},
		{"raised", days(30), days(60), ""},
		{"unknown", days(30), days(tftypes.UnknownValue), ""},
		{"shortened", days(30), days(7), "Retention cannot be shortened"},
	}
	for _, tc := range cases {
		got := planContainer(t, tc.prior, tc.planned)
		if tc.want == "" && got != "" {
			t.Errorf("%s: unexpected diagnostics: %s", tc.label, got)
		}
		if tc.want != "" && !strings.Contains(got, tc.want) {
			t.Errorf("%s: diagnostics %q, want %q", tc.label, got, tc.want)
		}
	}
}

func TestContainerObjectLockRefusesVersioningOff(t *testing.T) {
	config := func(versioning bool) map[string]tftypes.Value {
		return map[string]tftypes.Value{
			"name":           tftypes.NewValue(tftypes.String, "backups"),
			"versioning":     tftypes.NewValue(tftypes.Bool, versioning),
			"object_lock":    tftypes.NewValue(tftypes.Bool, true),
			"retention_days": tftypes.NewValue(tftypes.Number, 30),
		}
	}
	r := NewContainerResource().(resource.ResourceWithValidateConfig)
	if got := validate(t, r, config(true)); got != "" {
		t.Errorf("versioning on with object lock was refused: %s", got)
	}
	if got := validate(t, r, config(false)); !strings.Contains(got, "Versioning required by object lock") {
		t.Errorf("versioning off with object lock was admitted: %q", got)
	}
}

// S3 has no rename and the API refuses a name in a container PATCH: a new name
// is a new container, and the plan must say so.
func TestContainerNameChangeReplaces(t *testing.T) {
	ctx := context.Background()
	_, s := containerResourceFor(t, nil)
	attr, ok := s.Attributes["name"].(schema.StringAttribute)
	if !ok {
		t.Fatal("name missing")
	}
	stateRaw := containerState(t, s, nil)
	planRaw := containerState(t, s, map[string]tftypes.Value{"name": tftypes.NewValue(tftypes.String, "videos")})
	replace := false
	for _, modifier := range attr.PlanModifiers {
		req := planmodifier.StringRequest{
			Path:       tfpathRoot("name"),
			PlanValue:  types.StringValue("videos"),
			StateValue: types.StringValue("photos"),
			Plan:       tfsdk.Plan{Schema: s, Raw: planRaw},
			State:      tfsdk.State{Schema: s, Raw: stateRaw},
		}
		resp := planmodifier.StringResponse{PlanValue: req.PlanValue}
		modifier.PlanModifyString(ctx, req, &resp)
		replace = replace || resp.RequiresReplace
	}
	if !replace {
		t.Fatal("changing name must replace the container")
	}
}

// When omitted, the protection attributes keep their state on an update. If
// they were planned as unknown, RequiresReplace would see object_lock and
// retention_mode as changed and plan a replacement (deleting the data) for an
// unrelated change.
func TestContainerProtectionAttributesKeepTheirState(t *testing.T) {
	ctx := context.Background()
	_, s := containerResourceFor(t, nil)
	keepsState := func(descriptions []string) bool {
		for _, d := range descriptions {
			if strings.Contains(strings.ToLower(d), "will not change") {
				return true
			}
		}
		return false
	}
	descriptions := map[string][]string{}
	for name, attr := range s.Attributes {
		switch a := attr.(type) {
		case schema.BoolAttribute:
			for _, m := range a.PlanModifiers {
				descriptions[name] = append(descriptions[name], m.Description(ctx))
			}
		case schema.StringAttribute:
			for _, m := range a.PlanModifiers {
				descriptions[name] = append(descriptions[name], m.Description(ctx))
			}
		case schema.Int64Attribute:
			for _, m := range a.PlanModifiers {
				descriptions[name] = append(descriptions[name], m.Description(ctx))
			}
		}
	}
	for _, name := range []string{"versioning", "object_lock", "retention_mode", "retention_days"} {
		if !keepsState(descriptions[name]) {
			t.Errorf("%s must keep its state when omitted", name)
		}
	}
}
