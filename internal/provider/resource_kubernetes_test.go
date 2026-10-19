package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

// clusterAPI answers every write with a completed task and every cluster read
// with clusterJSON, recording what it was sent.
type clusterAPI struct {
	t           *testing.T
	requests    []string
	bodies      []map[string]any
	clusterJSON string
}

func (a *clusterAPI) client() *nemau.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/tasks/"):
			_, _ = w.Write([]byte(`{"id":"task-1","status":"completed"}`))
		case r.Method == http.MethodGet:
			a.requests = append(a.requests, r.Method+" "+r.URL.Path)
			_, _ = w.Write([]byte(a.clusterJSON))
		default:
			a.requests = append(a.requests, r.Method+" "+r.URL.Path)
			body := map[string]any{}
			_ = json.Unmarshal(raw, &body)
			a.bodies = append(a.bodies, body)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task_id":"task-1","status":"queued"}`))
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

const clusterReadJSON = `{"name":"prod","spec":{"topology":{"version":"v1.31.0","class":"nemau-default",
	"controlPlaneSizing":{"vcpus":2,"ram_mb":4096,"disk_gb":20},"controlPlaneReplicas":3,
	"nodePools":[{"name":"workers","class":"default-worker","vcpus":2,"ram_mb":4096,"disk_gb":20,"replicas":3},
	             {"name":"batch","class":"default-worker","vcpus":4,"ram_mb":8192,"disk_gb":50,"replicas":1}]}},
	"status":{"phase":"Provisioned","controlPlaneReady":true}}`

func clusterResource(t *testing.T, c *nemau.Client) (resource.Resource, schema.Schema) {
	t.Helper()
	r := NewKubernetesClusterResource()
	if c != nil {
		r.(resource.ResourceWithConfigure).Configure(context.Background(),
			resource.ConfigureRequest{ProviderData: c}, &resource.ConfigureResponse{})
	}
	var sr resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &sr)
	return r, sr.Schema
}

// objectValue builds a value of an object type from Go values: a string, an
// int, nil for null, or a map for a nested object. Attributes not named are
// null.
func objectValue(typ tftypes.Object, set map[string]any) tftypes.Value {
	if set == nil {
		return tftypes.NewValue(typ, nil)
	}
	values := map[string]tftypes.Value{}
	for name, attrType := range typ.AttributeTypes {
		switch v := set[name].(type) {
		case map[string]any:
			values[name] = objectValue(attrType.(tftypes.Object), v)
		case int:
			values[name] = tftypes.NewValue(attrType, int64(v))
		case nil:
			values[name] = tftypes.NewValue(attrType, nil)
		default:
			values[name] = tftypes.NewValue(attrType, v)
		}
	}
	return tftypes.NewValue(typ, values)
}

func clusterRaw(s schema.Schema, set map[string]any) tftypes.Value {
	return objectValue(s.Type().TerraformType(context.Background()).(tftypes.Object), set)
}

// validCluster is a configuration the API accepts, as a plan: computed values
// the practitioner left out are unknown there, not null.
func validCluster() map[string]any {
	return map[string]any{
		"name": "prod", "kubernetes_version": "v1.31.0",
		"control_plane_vcpus": 2, "control_plane_ram_mb": 4096, "control_plane_disk_gb": 20,
		"control_plane_replicas": 3,
		"default_node_pool": map[string]any{
			"name": "workers", "vcpus": 2, "ram_mb": 4096, "disk_gb": 20, "replicas": 3,
		},
	}
}

func validateCluster(t *testing.T, set map[string]any) []string {
	t.Helper()
	r, s := clusterResource(t, nil)
	var resp resource.ValidateConfigResponse
	r.(resource.ResourceWithValidateConfig).ValidateConfig(context.Background(),
		resource.ValidateConfigRequest{Config: tfsdk.Config{Schema: s, Raw: clusterRaw(s, set)}}, &resp)
	var summaries []string
	for _, d := range resp.Diagnostics.Errors() {
		summaries = append(summaries, d.Summary())
	}
	return summaries
}

func TestClusterConfigWithADefaultNodePoolIsValid(t *testing.T) {
	if errs := validateCluster(t, validCluster()); len(errs) != 0 {
		t.Fatalf("valid configuration refused: %v", errs)
	}
	// A pool given no size of its own takes the cluster's worker flavor.
	withWorkerFlavor := validCluster()
	withWorkerFlavor["worker_flavor_id"] = "flavor-1"
	withWorkerFlavor["default_node_pool"] = map[string]any{"name": "workers", "replicas": 0}
	if errs := validateCluster(t, withWorkerFlavor); len(errs) != 0 {
		t.Fatalf("pool sized by worker_flavor_id refused: %v", errs)
	}
}

// What the API refuses fails at plan time, before anything is created.
func TestClusterConfigRejectsWhatTheAPIRefuses(t *testing.T) {
	cases := map[string]struct {
		mutate func(map[string]any)
		want   string
	}{
		"no default node pool":        {func(c map[string]any) { delete(c, "default_node_pool") }, "Missing default node pool"},
		"zero control-plane replicas": {func(c map[string]any) { c["control_plane_replicas"] = 0 }, "Invalid control-plane replicas"},
		"ten control-plane replicas":  {func(c map[string]any) { c["control_plane_replicas"] = 10 }, "Invalid control-plane replicas"},
		"pool name not a DNS label":   {func(c map[string]any) { defaultPoolOf(c)["name"] = "Workers_1" }, "Invalid node pool name"},
		"pool above 100 replicas":     {func(c map[string]any) { defaultPoolOf(c)["replicas"] = 101 }, "Invalid node pool replicas"},
		"half an autoscaler":          {func(c map[string]any) { defaultPoolOf(c)["autoscaler_min"] = 1 }, "Incomplete autoscaler bounds"},
		"inverted autoscaler": {func(c map[string]any) {
			defaultPoolOf(c)["autoscaler_min"], defaultPoolOf(c)["autoscaler_max"] = 5, 2
		}, "Invalid autoscaler bounds"},
		"partial pool size":      {func(c map[string]any) { delete(defaultPoolOf(c), "disk_gb") }, "Incomplete node size"},
		"flavor and custom size": {func(c map[string]any) { defaultPoolOf(c)["flavor_id"] = "f" }, "Conflicting node size"},
		"no pool size at all": {func(c map[string]any) {
			c["default_node_pool"] = map[string]any{"name": "workers", "replicas": 3}
		}, "Missing node size"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			config := validCluster()
			tc.mutate(config)
			errs := validateCluster(t, config)
			if !strings.Contains(strings.Join(errs, "|"), tc.want) {
				t.Fatalf("errors = %v, want %q", errs, tc.want)
			}
		})
	}
}

func defaultPoolOf(config map[string]any) map[string]any {
	return config["default_node_pool"].(map[string]any)
}

// The create request carries the control-plane replicas and the default pool:
// the API refused it without them.
func TestClusterCreateSendsTheDefaultNodePool(t *testing.T) {
	api := &clusterAPI{t: t, clusterJSON: clusterReadJSON}
	r, s := clusterResource(t, api.client())

	plan := validCluster()
	defaultPoolOf(plan)["node_class"] = tftypes.UnknownValue
	defaultPoolOf(plan)["flavor_id"] = tftypes.UnknownValue
	plan["id"], plan["phase"], plan["endpoint"], plan["control_plane_ready"] =
		tftypes.UnknownValue, tftypes.UnknownValue, tftypes.UnknownValue, tftypes.UnknownValue
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(context.Background()), nil)}}
	r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Schema: s, Raw: clusterRaw(s, plan)}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}

	if len(api.bodies) != 1 || api.requests[0] != "POST /api/v1/kubernetes/clusters" {
		t.Fatalf("requests = %v", api.requests)
	}
	body := api.bodies[0]
	if body["control_plane_replicas"] != float64(3) {
		t.Errorf("control_plane_replicas = %v", body["control_plane_replicas"])
	}
	pools, _ := body["node_pools"].([]any)
	if len(pools) != 1 {
		t.Fatalf("node_pools = %v", body["node_pools"])
	}
	sent := pools[0].(map[string]any)
	if sent["name"] != "workers" || sent["class"] != defaultNodeClass || sent["replicas"] != float64(3) ||
		sent["vcpus"] != float64(2) || sent["flavor_id"] != nil {
		t.Errorf("pool = %v", sent)
	}

	var state kubernetesClusterModel
	if diags := resp.State.Get(context.Background(), &state); diags.HasError() {
		t.Fatalf("state: %v", diags)
	}
	// Read back by name, not as the first pool.
	if p := state.DefaultNodePool; p == nil || p.Name.ValueString() != "workers" ||
		p.NodeClass.ValueString() != "default-worker" || !p.FlavorID.IsNull() || p.Replicas.ValueInt64() != 3 {
		t.Errorf("default pool in state = %+v", state.DefaultNodePool)
	}
}

// Scaling the default pool scales it in place; it does not touch the cluster.
func TestClusterUpdateScalesTheDefaultPoolInPlace(t *testing.T) {
	api := &clusterAPI{t: t, clusterJSON: clusterReadJSON}
	r, s := clusterResource(t, api.client())

	state := validCluster()
	state["id"], state["phase"], state["endpoint"], state["control_plane_ready"] = "prod", "Provisioned", "", true
	state["cluster_class"] = "nemau-default"
	defaultPoolOf(state)["node_class"] = "default-worker"
	plan := validCluster()
	plan["id"], plan["phase"], plan["endpoint"], plan["control_plane_ready"] = "prod", "Provisioned", "", true
	plan["cluster_class"] = "nemau-default"
	defaultPoolOf(plan)["node_class"] = "default-worker"
	defaultPoolOf(plan)["replicas"] = 5
	defaultPoolOf(plan)["autoscaler_min"], defaultPoolOf(plan)["autoscaler_max"] = 2, 8

	resp := resource.UpdateResponse{State: tfsdk.State{Schema: s, Raw: clusterRaw(s, state)}}
	r.Update(context.Background(), resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: s, Raw: clusterRaw(s, plan)},
		State: tfsdk.State{Schema: s, Raw: clusterRaw(s, state)},
	}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	want := []string{
		"POST /api/v1/kubernetes/clusters/prod/node-pools/workers/scale",
		"POST /api/v1/kubernetes/clusters/prod/node-pools/workers/autoscaler",
		"GET /api/v1/kubernetes/clusters/prod",
	}
	if strings.Join(api.requests, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(api.requests, "\n"), strings.Join(want, "\n"))
	}
	if api.bodies[0]["replicas"] != float64(5) || api.bodies[1]["min"] != float64(2) || api.bodies[1]["max"] != float64(8) {
		t.Errorf("bodies = %v", api.bodies)
	}
}

// An imported cluster has no default pool in state: it adopts the first one,
// the pool created with the cluster.
func TestClusterImportAdoptsTheFirstPool(t *testing.T) {
	model := kubernetesClusterModel{}
	var cluster nemau.KubernetesCluster
	if err := json.Unmarshal([]byte(clusterReadJSON), &cluster); err != nil {
		t.Fatal(err)
	}
	applyCluster(&model, &cluster)
	if p := model.DefaultNodePool; p == nil || p.Name.ValueString() != "workers" || p.VCPUs.ValueInt64() != 2 {
		t.Fatalf("default pool = %+v", model.DefaultNodePool)
	}
}

// A version change is an in-place guided upgrade: one call carrying exactly
// {"version": ...}, no preflight or window created by the provider, and never a
// replacement.
func TestClusterUpdateUpgradesInPlace(t *testing.T) {
	upgraded := strings.Replace(clusterReadJSON, `"version":"v1.31.0"`, `"version":"v1.32.1"`, 1)
	api := &clusterAPI{t: t, clusterJSON: upgraded}
	r, s := clusterResource(t, api.client())

	state := validCluster()
	state["id"], state["phase"], state["endpoint"], state["control_plane_ready"] = "prod", "Provisioned", "", true
	state["cluster_class"] = "nemau-default"
	defaultPoolOf(state)["node_class"] = "default-worker"
	plan := validCluster()
	plan["id"], plan["phase"], plan["endpoint"], plan["control_plane_ready"] = "prod", "Provisioned", "", true
	plan["cluster_class"] = "nemau-default"
	plan["kubernetes_version"] = "1.32.1" // the bare form the API also accepts
	defaultPoolOf(plan)["node_class"] = "default-worker"

	resp := resource.UpdateResponse{State: tfsdk.State{Schema: s, Raw: clusterRaw(s, state)}}
	r.Update(context.Background(), resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: s, Raw: clusterRaw(s, plan)},
		State: tfsdk.State{Schema: s, Raw: clusterRaw(s, state)},
	}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	want := []string{
		"POST /api/v1/kubernetes/clusters/prod/upgrade",
		"GET /api/v1/kubernetes/clusters/prod",
	}
	if strings.Join(api.requests, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(api.requests, "\n"), strings.Join(want, "\n"))
	}
	if len(api.bodies[0]) != 1 || api.bodies[0]["version"] != "1.32.1" {
		t.Errorf("upgrade body = %v", api.bodies[0])
	}

	var got kubernetesClusterModel
	resp.Diagnostics.Append(resp.State.Get(context.Background(), &got)...)
	// The spelling the configuration used survives the read-back, or Terraform
	// would fail the apply with "inconsistent result after apply".
	if got.KubernetesVersion.ValueString() != "1.32.1" {
		t.Errorf("kubernetes_version in state = %q, want the configured spelling", got.KubernetesVersion.ValueString())
	}
}

// A version that differs only in spelling is not a change, so nothing is sent.
func TestClusterUpdateIgnoresAVersionSpellingChange(t *testing.T) {
	api := &clusterAPI{t: t, clusterJSON: clusterReadJSON}
	r, s := clusterResource(t, api.client())

	state := validCluster() // v1.31.0
	state["id"], state["phase"], state["endpoint"], state["control_plane_ready"] = "prod", "Provisioned", "", true
	state["cluster_class"] = "nemau-default"
	defaultPoolOf(state)["node_class"] = "default-worker"
	plan := validCluster()
	plan["id"], plan["phase"], plan["endpoint"], plan["control_plane_ready"] = "prod", "Provisioned", "", true
	plan["cluster_class"] = "nemau-default"
	plan["kubernetes_version"] = "1.31.0"
	defaultPoolOf(plan)["node_class"] = "default-worker"

	resp := resource.UpdateResponse{State: tfsdk.State{Schema: s, Raw: clusterRaw(s, state)}}
	r.Update(context.Background(), resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: s, Raw: clusterRaw(s, plan)},
		State: tfsdk.State{Schema: s, Raw: clusterRaw(s, state)},
	}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	for _, req := range api.requests {
		if strings.HasPrefix(req, "POST") {
			t.Errorf("a spelling change sent %s", req)
		}
	}
}
