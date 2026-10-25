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
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

// instanceUpdateAPI stands in for the instance routes a rename, a create and a
// stop touch: the create POST, the PATCH, the stop PUT, their tasks and the read
// that follows. A task listed in failing is reported as failed.
type instanceUpdateAPI struct {
	t *testing.T

	mu          sync.Mutex
	name        string
	description string
	backup      map[string]any // backup_* fields the last PATCH set
	patches     []map[string]any
	creates     int
	stops       int
	failing     map[string]bool // task ids reported as failed
}

func (a *instanceUpdateAPI) client() *nemau.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "PATCH /api/v1/compute/instances/i-1":
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				a.t.Errorf("PATCH body %q: %v", raw, err)
			}
			a.patches = append(a.patches, body)
			// A PATCH whose task fails changes nothing on the platform.
			if !a.failing["t-1"] {
				if v, ok := body["name"].(string); ok {
					a.name = v
				}
				if v, ok := body["description"].(string); ok {
					a.description = v
				}
				if a.backup == nil {
					a.backup = map[string]any{}
				}
				for k, v := range body {
					if strings.HasPrefix(k, "backup_") {
						a.backup[k] = v
					}
				}
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task_id":"t-1","status":"pending"}`))
		case "POST /api/v1/compute/instances":
			a.creates++
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task_id":"t-create","status":"pending"}`))
		case "PUT /api/v1/compute/instances/i-1/stop":
			a.stops++
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task_id":"t-stop","status":"pending"}`))
		case "GET /api/v1/tasks/t-create":
			_, _ = w.Write([]byte(a.task("t-create", `"resourceId":"i-1",`)))
		case "GET /api/v1/tasks/t-1", "GET /api/v1/tasks/t-stop":
			_, _ = w.Write([]byte(a.task(r.URL.Path[len("/api/v1/tasks/"):], "")))
		case "GET /api/v1/compute/instances/i-1":
			instance := map[string]any{
				"id": "i-1", "name": a.name, "description": a.description, "status": "running",
				"sizing": map[string]any{"vcpus": 1, "ram_mb": 1024, "disk_gb": 10},
				"image":  map[string]any{"id": "img-1"},
			}
			for k, v := range a.backup {
				instance[k] = v
			}
			out, _ := json.Marshal(map[string]any{"instance": instance})
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

// task renders a finished task: failed when listed in failing, completed
// otherwise. extra is spliced in as JSON fields before the status.
func (a *instanceUpdateAPI) task(id, extra string) string {
	if a.failing[id] {
		return `{"id":"` + id + `","status":"failed","error":"platform refused the change"}`
	}
	return `{"id":"` + id + `",` + extra + `"status":"completed","progress":100}`
}

func (a *instanceUpdateAPI) sent() []map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]map[string]any(nil), a.patches...)
}

func instanceResourceFor(t *testing.T, c *nemau.Client) (resource.Resource, schema.Schema) {
	t.Helper()
	r := NewInstanceResource()
	r.(resource.ResourceWithConfigure).Configure(context.Background(),
		resource.ConfigureRequest{ProviderData: c}, &resource.ConfigureResponse{})
	var sr resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &sr)
	return r, sr.Schema
}

// webInstance is a running 1 vCPU instance named "web", with the given values
// overriding the defaults and every other attribute null.
func webInstance(t *testing.T, s schema.Schema, set map[string]tftypes.Value) tftypes.Value {
	t.Helper()
	typ := s.Type().TerraformType(context.Background()).(tftypes.Object)
	values := map[string]tftypes.Value{}
	for name, attrType := range typ.AttributeTypes {
		values[name] = tftypes.NewValue(attrType, nil)
	}
	values["id"] = tftypes.NewValue(tftypes.String, "i-1")
	values["name"] = tftypes.NewValue(tftypes.String, "web")
	values["description"] = tftypes.NewValue(tftypes.String, "front")
	values["image_id"] = tftypes.NewValue(tftypes.String, "img-1")
	values["vcpus"] = tftypes.NewValue(tftypes.Number, 1)
	values["ram_mb"] = tftypes.NewValue(tftypes.Number, 1024)
	values["disk_gb"] = tftypes.NewValue(tftypes.Number, 10)
	values["started"] = tftypes.NewValue(tftypes.Bool, true)
	values["status"] = tftypes.NewValue(tftypes.String, "running")
	values["addresses"] = tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, []tftypes.Value{})
	for k, v := range set {
		values[k] = v
	}
	return tftypes.NewValue(typ, values)
}

func updateInstance(t *testing.T, api *instanceUpdateAPI, prior, planned tftypes.Value) (instanceModel, string) {
	t.Helper()
	r, s := instanceResourceFor(t, api.client())
	state := tfsdk.State{Schema: s, Raw: prior}
	resp := resource.UpdateResponse{State: state}
	r.Update(context.Background(), resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: s, Raw: planned},
		State: state,
	}, &resp)
	var m instanceModel
	if diags := resp.State.Get(context.Background(), &m); diags.HasError() {
		t.Fatal(diags)
	}
	messages := make([]string, 0, len(resp.Diagnostics))
	for _, d := range resp.Diagnostics {
		messages = append(messages, d.Summary()+": "+d.Detail())
	}
	return m, strings.Join(messages, "\n")
}

// A new name reaches the API, alone, and the state reports it.
func TestInstanceUpdateRenames(t *testing.T) {
	api := &instanceUpdateAPI{t: t, name: "web", description: "front"}
	_, s := instanceResourceFor(t, nil)
	m, diags := updateInstance(t, api, webInstance(t, s, nil),
		webInstance(t, s, map[string]tftypes.Value{"name": tftypes.NewValue(tftypes.String, "web-2")}))
	if diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	if sent := api.sent(); len(sent) != 1 || len(sent[0]) != 1 || sent[0]["name"] != "web-2" {
		t.Fatalf("PATCH bodies = %v, want one {name: web-2}", sent)
	}
	if m.Name.ValueString() != "web-2" || m.Description.ValueString() != "front" {
		t.Errorf("state = name %v, description %v", m.Name, m.Description)
	}
}

// Removing the description from the configuration clears it on the instance.
func TestInstanceUpdateClearsTheDescription(t *testing.T) {
	api := &instanceUpdateAPI{t: t, name: "web", description: "front"}
	_, s := instanceResourceFor(t, nil)
	m, diags := updateInstance(t, api, webInstance(t, s, nil),
		webInstance(t, s, map[string]tftypes.Value{"description": tftypes.NewValue(tftypes.String, nil)}))
	if diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	if sent := api.sent(); len(sent) != 1 || len(sent[0]) != 1 || sent[0]["description"] != "" {
		t.Fatalf("PATCH bodies = %v, want one {description: \"\"}", sent)
	}
	if !m.Description.IsNull() {
		t.Errorf("description = %v, want null", m.Description)
	}
}

// When neither the name nor the description changes, no PATCH is sent.
func TestInstanceUpdateWithoutLabelChangeSendsNoPatch(t *testing.T) {
	api := &instanceUpdateAPI{t: t, name: "web", description: "front"}
	_, s := instanceResourceFor(t, nil)
	_, diags := updateInstance(t, api, webInstance(t, s, nil), webInstance(t, s, nil))
	if diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	if sent := api.sent(); len(sent) != 0 {
		t.Fatalf("PATCH sent: %v", sent)
	}
}
