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
// Placeholder identifiers: the network and subnet of a cluster are UUIDs the
// platform owns; these values name no real resource.
const (
	exampleNetworkID = "3f2b8c1e-5d4a-4e7b-9c1d-0a1b2c3d4e5f"
	exampleSubnetID  = "7a9e0b2c-1d3f-4a5b-8c6d-2e4f6a8b0c1d"
)

func validCluster() map[string]any {
	return map[string]any{
		"name": "prod", "kubernetes_version": "v1.31.0",
		"network_id": exampleNetworkID, "subnet_id": exampleSubnetID,
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

// The create request carries the tenant network, the optional CIDRs and the
// pool's labels and taints. Unset CIDRs are left out, not sent empty.
func TestClusterCreateSendsTheTenantNetwork(t *testing.T) {
	api := &clusterAPI{t: t, clusterJSON: clusterReadJSON}
	r, s := clusterResource(t, api.client())

	plan := validCluster()
	plan["pod_cidr"] = "10.244.0.0/16"
	plan["service_cidr"] = tftypes.UnknownValue
	defaultPoolOf(plan)["node_class"] = tftypes.UnknownValue
	defaultPoolOf(plan)["flavor_id"] = tftypes.UnknownValue
	defaultPoolOf(plan)["labels"] = []tftypes.Value{tftypes.NewValue(tftypes.String, "team=platform")}
	defaultPoolOf(plan)["taints"] = []tftypes.Value{tftypes.NewValue(tftypes.String, "dedicated=gpu:NoSchedule")}
	plan["id"], plan["phase"], plan["endpoint"], plan["control_plane_ready"] =
		tftypes.UnknownValue, tftypes.UnknownValue, tftypes.UnknownValue, tftypes.UnknownValue
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(context.Background()), nil)}}
	r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Schema: s, Raw: clusterRaw(s, plan)}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}

	body := api.bodies[0]
	if body["network_id"] != exampleNetworkID || body["subnet_id"] != exampleSubnetID {
		t.Errorf("network = %v / %v", body["network_id"], body["subnet_id"])
	}
	if body["pod_cidr"] != "10.244.0.0/16" {
		t.Errorf("pod_cidr = %v", body["pod_cidr"])
	}
	if _, sent := body["service_cidr"]; sent {
		t.Errorf("unset service_cidr was sent: %v", body["service_cidr"])
	}
	sent := body["node_pools"].([]any)[0].(map[string]any)
	if labels, _ := sent["labels"].([]any); len(labels) != 1 || labels[0] != "team=platform" {
		t.Errorf("labels = %v", sent["labels"])
	}
	if taints, _ := sent["taints"].([]any); len(taints) != 1 || taints[0] != "dedicated=gpu:NoSchedule" {
		t.Errorf("taints = %v", sent["taints"])
	}

	var state kubernetesClusterModel
	if diags := resp.State.Get(context.Background(), &state); diags.HasError() {
		t.Fatalf("state: %v", diags)
	}
	if state.NetworkID.ValueString() != exampleNetworkID || state.PodCIDR.ValueString() != "10.244.0.0/16" {
		t.Errorf("state network = %q, pod = %q", state.NetworkID.ValueString(), state.PodCIDR.ValueString())
	}
	if !state.ServiceCIDR.IsNull() {
		t.Errorf("service_cidr = %v, want null", state.ServiceCIDR)
	}
}

// A read keeps the network the state holds when the API reports none, and
// never invents a value.
func TestClusterReadKeepsTheNetworkFromState(t *testing.T) {
	api := &clusterAPI{t: t, clusterJSON: clusterReadJSON}
	r, s := clusterResource(t, api.client())

	state := validCluster()
	state["id"], state["phase"], state["endpoint"], state["control_plane_ready"] = "prod", "Provisioned", "", true
	state["pod_cidr"] = "10.244.0.0/16"
	state["service_cidr"] = nil
	defaultPoolOf(state)["node_class"] = "default-worker"

	resp := resource.ReadResponse{State: tfsdk.State{Schema: s, Raw: clusterRaw(s, state)}}
	r.Read(context.Background(), resource.ReadRequest{State: tfsdk.State{Schema: s, Raw: clusterRaw(s, state)}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics)
	}
	var got kubernetesClusterModel
	if diags := resp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("state: %v", diags)
	}
	if got.NetworkID.ValueString() != exampleNetworkID || got.SubnetID.ValueString() != exampleSubnetID {
		t.Errorf("network dropped by read: %q / %q", got.NetworkID.ValueString(), got.SubnetID.ValueString())
	}
	if got.PodCIDR.ValueString() != "10.244.0.0/16" || !got.ServiceCIDR.IsNull() {
		t.Errorf("CIDRs = %q / %v", got.PodCIDR.ValueString(), got.ServiceCIDR)
	}
}

// tenantClusterJSON is the cluster view with the tenant network and the pool
// labels and taints the API reports (the API's read view).
const tenantClusterJSON = `{"name":"prod","spec":{"topology":{"version":"v1.31.0","class":"nemau-default",
	"controlPlaneSizing":{"vcpus":2,"ram_mb":4096,"disk_gb":20},"controlPlaneReplicas":3,
	"nodePools":[{"name":"workers","class":"default-worker","vcpus":2,"ram_mb":4096,"disk_gb":20,"replicas":3,
	              "labels":["workload=general"],"taints":["dedicated=general:NoSchedule"]},
	             {"name":"batch","class":"default-worker","vcpus":4,"ram_mb":8192,"disk_gb":50,"replicas":1,
	              "labels":[],"taints":[]}]},
	"network_id":"11111111-1111-4111-8111-111111111111","subnet_id":"22222222-2222-4222-8222-222222222222",
	"pod_cidr":"10.245.0.0/16","service_cidr":"10.96.0.0/12"},
	"status":{"phase":"Provisioned","controlPlaneReady":true}}`

// The read takes the network, the CIDRs and the pool labels and taints from the
// API, over what the state held: a read is authoritative for them.
func TestClusterReadTakesTheTenantNetworkFromTheAPI(t *testing.T) {
	api := &clusterAPI{t: t, clusterJSON: tenantClusterJSON}
	r, s := clusterResource(t, api.client())

	state := validCluster()
	state["id"], state["phase"], state["endpoint"], state["control_plane_ready"] = "prod", "Provisioned", "", true
	state["pod_cidr"] = "10.99.0.0/16"
	state["service_cidr"] = nil
	defaultPoolOf(state)["node_class"] = "default-worker"

	resp := resource.ReadResponse{State: tfsdk.State{Schema: s, Raw: clusterRaw(s, state)}}
	r.Read(context.Background(), resource.ReadRequest{State: tfsdk.State{Schema: s, Raw: clusterRaw(s, state)}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics)
	}
	var got kubernetesClusterModel
	if diags := resp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("state: %v", diags)
	}
	if got.NetworkID.ValueString() != "11111111-1111-4111-8111-111111111111" ||
		got.SubnetID.ValueString() != "22222222-2222-4222-8222-222222222222" {
		t.Errorf("network = %q / %q, want the API's", got.NetworkID.ValueString(), got.SubnetID.ValueString())
	}
	if got.PodCIDR.ValueString() != "10.245.0.0/16" || got.ServiceCIDR.ValueString() != "10.96.0.0/12" {
		t.Errorf("CIDRs = %q / %q, want the API's", got.PodCIDR.ValueString(), got.ServiceCIDR.ValueString())
	}
	if pool := got.DefaultNodePool; pool == nil || !sameStringSet(pool.Labels, []string{"workload=general"}) ||
		!sameStringSet(pool.Taints, []string{"dedicated=general:NoSchedule"}) {
		t.Errorf("default pool labels/taints = %+v", got.DefaultNodePool)
	}
}

// An import has no state to keep: the network comes from the API alone.
func TestClusterImportReadsTheTenantNetwork(t *testing.T) {
	api := &clusterAPI{t: t, clusterJSON: tenantClusterJSON}
	r, s := clusterResource(t, api.client())

	state := map[string]any{"id": "prod", "name": "prod"}
	resp := resource.ReadResponse{State: tfsdk.State{Schema: s, Raw: clusterRaw(s, state)}}
	r.Read(context.Background(), resource.ReadRequest{State: tfsdk.State{Schema: s, Raw: clusterRaw(s, state)}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics)
	}
	var got kubernetesClusterModel
	if diags := resp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("state: %v", diags)
	}
	if got.NetworkID.ValueString() != "11111111-1111-4111-8111-111111111111" || got.ServiceCIDR.ValueString() != "10.96.0.0/12" {
		t.Errorf("imported network = %q, service = %q", got.NetworkID.ValueString(), got.ServiceCIDR.ValueString())
	}
}

// A node pool added with labels and taints sends them, and keeps them in state.
func TestNodePoolCreateSendsLabelsAndTaints(t *testing.T) {
	api := &clusterAPI{t: t, clusterJSON: tenantClusterJSON}
	c := api.client()
	r := NewKubernetesNodePoolResource()
	r.(resource.ResourceWithConfigure).Configure(context.Background(),
		resource.ConfigureRequest{ProviderData: c}, &resource.ConfigureResponse{})
	var sr resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &sr)
	s := sr.Schema

	plan := map[string]any{
		"id": tftypes.UnknownValue, "cluster_name": "prod", "name": "gpu", "node_class": "default-worker",
		"flavor_id": "7d2e9a10-4b5c-4e8f-a1b2-c3d4e5f60718", "vcpus": tftypes.UnknownValue,
		"ram_mb": tftypes.UnknownValue, "disk_gb": tftypes.UnknownValue, "replicas": 1,
		"autoscaler_min": nil, "autoscaler_max": nil,
		"labels": []tftypes.Value{tftypes.NewValue(tftypes.String, "accelerator=gpu")},
		"taints": []tftypes.Value{tftypes.NewValue(tftypes.String, "dedicated=gpu:NoSchedule")},
	}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(context.Background()), nil)}}
	r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Schema: s, Raw: clusterRaw(s, plan)}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}

	body := api.bodies[0]
	if labels, _ := body["labels"].([]any); len(labels) != 1 || labels[0] != "accelerator=gpu" {
		t.Errorf("labels sent = %v", body["labels"])
	}
	if taints, _ := body["taints"].([]any); len(taints) != 1 || taints[0] != "dedicated=gpu:NoSchedule" {
		t.Errorf("taints sent = %v", body["taints"])
	}
	var got kubernetesNodePoolModel
	if diags := resp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("state: %v", diags)
	}
	if !sameStringSet(got.Labels, []string{"accelerator=gpu"}) || !sameStringSet(got.Taints, []string{"dedicated=gpu:NoSchedule"}) {
		t.Errorf("state labels = %v, taints = %v", got.Labels, got.Taints)
	}
}
