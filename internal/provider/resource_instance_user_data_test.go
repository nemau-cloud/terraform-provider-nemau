package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

const webDocument = "#cloud-config\nruncmd:\n  - echo hello\n"

// userDataAPI stands in for the create route, its task, and the instance read
// that follows. The instance reports the SHA-256 the test sets.
type userDataAPI struct {
	t *testing.T

	mu     sync.Mutex
	sha256 string
	bodies []map[string]any
}

func (a *userDataAPI) client() *nemau.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/compute/instances":
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				a.t.Errorf("POST body: %v", err)
			}
			a.bodies = append(a.bodies, body)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task_id":"t-1","status":"pending"}`))
		case "GET /api/v1/tasks/t-1":
			_, _ = w.Write([]byte(`{"id":"t-1","status":"completed","progress":100,"result":{"instance_id":"i-1"}}`))
		case "GET /api/v1/compute/instances/i-1":
			out, _ := json.Marshal(map[string]any{"instance": map[string]any{
				"id": "i-1", "name": "web", "status": "running",
				"sizing":           map[string]any{"vcpus": 1, "ram_mb": 1024, "disk_gb": 10},
				"image":            map[string]any{"id": "img-1"},
				"user_data_sha256": a.sha256,
			}})
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

func (a *userDataAPI) sent() []map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]map[string]any(nil), a.bodies...)
}

// userDataInstance is an instance state with user_data set to value (null when
// value is nil) and every other attribute the instance read fills.
func userDataInstance(t *testing.T, s schema.Schema, value tftypes.Value) tftypes.Value {
	t.Helper()
	return webInstance(t, s, map[string]tftypes.Value{"user_data": value})
}

func userDataValue(doc string) tftypes.Value {
	return tftypes.NewValue(tftypes.String, doc)
}

func TestInstanceCreateSendsUserDataWhenConfigured(t *testing.T) {
	api := &userDataAPI{t: t, sha256: userDataFingerprint(webDocument)}
	r, s := instanceResourceFor(t, api.client())

	plan := webInstance(t, s, map[string]tftypes.Value{
		"id":        tftypes.NewValue(tftypes.String, nil),
		"user_data": userDataValue(webDocument),
	})
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(context.Background()), nil)}}
	r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Schema: s, Raw: plan}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Create: %v", resp.Diagnostics)
	}
	sent := api.sent()
	if len(sent) != 1 || sent[0]["user_data"] != webDocument {
		t.Fatalf("POST bodies = %v, want user_data set to the document", sent)
	}
}

func TestInstanceCreateOmitsUserDataWhenNotConfigured(t *testing.T) {
	api := &userDataAPI{t: t}
	r, s := instanceResourceFor(t, api.client())

	plan := webInstance(t, s, map[string]tftypes.Value{"id": tftypes.NewValue(tftypes.String, nil)})
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(context.Background()), nil)}}
	r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Schema: s, Raw: plan}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Create: %v", resp.Diagnostics)
	}
	sent := api.sent()
	if len(sent) != 1 {
		t.Fatalf("POST bodies = %v", sent)
	}
	if _, present := sent[0]["user_data"]; present {
		t.Errorf("user_data sent without a configured value: %v", sent[0])
	}
}

// The state keeps the configured document, and nothing changes while the
// platform reports the same SHA-256. A byte order mark is not part of the
// fingerprint: the platform strips it before it hashes.
func TestInstanceReadKeepsUserDataWhenTheFingerprintMatches(t *testing.T) {
	for _, doc := range []string{webDocument, "\uFEFF" + webDocument} {
		api := &userDataAPI{t: t, sha256: userDataFingerprint(doc)}
		r, s := instanceResourceFor(t, api.client())
		prior := userDataInstance(t, s, userDataValue(doc))
		resp := resource.ReadResponse{State: tfsdk.State{Schema: s, Raw: prior}}
		r.Read(context.Background(), resource.ReadRequest{State: tfsdk.State{Schema: s, Raw: prior}}, &resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("Read: %v", resp.Diagnostics)
		}
		var m instanceModel
		if diags := resp.State.Get(context.Background(), &m); diags.HasError() {
			t.Fatal(diags)
		}
		if m.UserData.ValueString() != doc {
			t.Errorf("user_data dropped from the state although the fingerprint matches (doc %q)", doc)
		}
	}
}

// A document the platform no longer reports as the one applied is dropped from
// the state, so the plan replaces the instance.
func TestInstanceReadDropsUserDataOnFingerprintDrift(t *testing.T) {
	api := &userDataAPI{t: t, sha256: userDataFingerprint("#cloud-config\nother: true\n")}
	r, s := instanceResourceFor(t, api.client())
	prior := userDataInstance(t, s, userDataValue(webDocument))
	resp := resource.ReadResponse{State: tfsdk.State{Schema: s, Raw: prior}}
	r.Read(context.Background(), resource.ReadRequest{State: tfsdk.State{Schema: s, Raw: prior}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", resp.Diagnostics)
	}
	var m instanceModel
	if diags := resp.State.Get(context.Background(), &m); diags.HasError() {
		t.Fatal(diags)
	}
	if !m.UserData.IsNull() {
		t.Errorf("user_data = %v after drift, want null", m.UserData)
	}
}

// After an import the value is unknown to Terraform, and the platform's
// fingerprint is not compared with anything: the state holds null.
func TestInstanceReadLeavesAnImportedUserDataNull(t *testing.T) {
	api := &userDataAPI{t: t, sha256: userDataFingerprint(webDocument)}
	r, s := instanceResourceFor(t, api.client())
	prior := userDataInstance(t, s, tftypes.NewValue(tftypes.String, nil))
	resp := resource.ReadResponse{State: tfsdk.State{Schema: s, Raw: prior}}
	r.Read(context.Background(), resource.ReadRequest{State: tfsdk.State{Schema: s, Raw: prior}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", resp.Diagnostics)
	}
	var m instanceModel
	if diags := resp.State.Get(context.Background(), &m); diags.HasError() {
		t.Fatal(diags)
	}
	if !m.UserData.IsNull() {
		t.Errorf("user_data = %v, want null", m.UserData)
	}
}

// The document is a secret: Terraform must hide it, and a change must replace
// the instance rather than update it, because cloud-init reads it once.
func TestInstanceUserDataIsSensitiveAndForcesReplacement(t *testing.T) {
	_, s := instanceResourceFor(t, nil)
	attr, ok := s.Attributes["user_data"].(schema.StringAttribute)
	if !ok {
		t.Fatalf("user_data is %T, want a string attribute", s.Attributes["user_data"])
	}
	if !attr.Sensitive || !attr.Optional || attr.Computed {
		t.Errorf("user_data: sensitive=%v optional=%v computed=%v", attr.Sensitive, attr.Optional, attr.Computed)
	}
	// The framework's modifier only acts on an existing resource, so the
	// request needs a state and a plan that are not null.
	raw := webInstance(t, s, map[string]tftypes.Value{"user_data": userDataValue("old")})
	state := tfsdk.State{Schema: s, Raw: raw}
	plan := tfsdk.Plan{Schema: s, Raw: raw}
	var replaces bool
	for _, m := range attr.PlanModifiers {
		resp := &planmodifier.StringResponse{}
		m.PlanModifyString(context.Background(), planmodifier.StringRequest{
			Path:        path.Root("user_data"),
			State:       state,
			Plan:        plan,
			StateValue:  types.StringValue("old"),
			PlanValue:   types.StringValue("new"),
			ConfigValue: types.StringValue("new"),
		}, resp)
		replaces = replaces || resp.RequiresReplace
	}
	if !replaces {
		t.Error("a changed user_data does not require replacement")
	}
}

func TestUserDataFingerprintIsSHA256OfTheNormalizedDocument(t *testing.T) {
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" // sha256("abc")
	if got := userDataFingerprint("abc"); got != want {
		t.Errorf("fingerprint = %s", got)
	}
	if userDataFingerprint("\uFEFFabc") != want {
		t.Error("the byte order mark is part of the fingerprint")
	}
}

// An empty value is never sent, so the platform holds no fingerprint for it:
// that is not drift, and it must not replace the instance on every plan.
func TestUserDataDriftIgnoresEmptyValues(t *testing.T) {
	if userDataDrifted(types.StringValue(""), "") {
		t.Error("an empty value with no platform fingerprint is drift")
	}
	if userDataDrifted(types.StringValue(""), userDataFingerprint("")) {
		t.Error("an empty value is drift")
	}
}

// A platform that reports no fingerprint, for a configured document, is not
// drift either: a read that loses the fingerprint must not replace a live
// instance.
func TestUserDataDriftIgnoresAMissingFingerprint(t *testing.T) {
	if userDataDrifted(types.StringValue(webDocument), "") {
		t.Error("a missing platform fingerprint replaces the instance")
	}
	if !userDataDrifted(types.StringValue(webDocument), userDataFingerprint("other")) {
		t.Error("a different fingerprint is not drift")
	}
}
