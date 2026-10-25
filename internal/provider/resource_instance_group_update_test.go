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

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

const groupPath = "/api/v1/compute/instance-groups/g-1"

// groupStub stands in for one instance group: its GET answers the stored view,
// its PATCH and capacity PUT change it and are recorded in order.
type groupStub struct {
	refreshes int // POST refresh requests received
	t         *testing.T

	mu       sync.Mutex
	group    nemau.InstanceGroup
	requests []string
	patches  []map[string]any
	// rejectCapacity makes the platform refuse every SetCapacity.
	rejectCapacity bool
	// rejectPatch makes the platform refuse every PATCH. A refused PATCH is not recorded.
	rejectPatch bool
}

func (g *groupStub) client() *nemau.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.requests = append(g.requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/t-1":
			_, _ = w.Write([]byte(`{"id":"t-1","status":"completed","progress":100,"resourceId":"g-1"}`))
		case r.Method == http.MethodGet && r.URL.Path == groupPath:
			out, _ := json.Marshal(map[string]any{"instance_group": g.group})
			_, _ = w.Write(out)
		case r.Method == http.MethodPatch && r.URL.Path == groupPath:
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				g.t.Errorf("PATCH body is not JSON: %v", err)
			}
			if g.rejectPatch {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"error":"launch template refused"}`))
				return
			}
			g.patches = append(g.patches, body)
			// As the platform checks it: the desired capacity must lie within the new bounds.
			minSize, maxSize := g.group.MinSize, g.group.MaxSize
			if v, ok := body["min_size"].(float64); ok {
				minSize = int(v)
			}
			if v, ok := body["max_size"].(float64); ok {
				maxSize = int(v)
			}
			if g.group.DesiredCapacity < minSize || g.group.DesiredCapacity > maxSize {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"error":"desired_capacity must stay between min_size and max_size"}`))
				return
			}
			g.group.MinSize, g.group.MaxSize = minSize, maxSize
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task_id":"t-1","status":"pending"}`))
		case r.Method == http.MethodPut && r.URL.Path == groupPath+"/capacity":
			if g.rejectCapacity {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"error":"desired_capacity refused"}`))
				return
			}
			raw, _ := io.ReadAll(r.Body)
			var body struct {
				DesiredCapacity int `json:"desired_capacity"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				g.t.Errorf("capacity body is not JSON: %v", err)
			}
			// As the platform checks it: against the group's current bounds, not the ones a change is about to set.
			if body.DesiredCapacity < g.group.MinSize || body.DesiredCapacity > g.group.MaxSize {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"error":"desired_capacity must be between the group's min_size and max_size"}`))
				return
			}
			g.group.DesiredCapacity = body.DesiredCapacity
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task_id":"t-1","status":"pending"}`))
		case r.Method == http.MethodPost && r.URL.Path == groupPath+"/refresh":
			g.refreshes++
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task_id":"t-1","status":"pending"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/compute/instance-groups":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task_id":"t-1","status":"pending"}`))
		default:
			g.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	g.t.Cleanup(srv.Close)
	c, err := nemau.NewClient(
		nemau.WithBaseURL(srv.URL),
		nemau.WithCredential(nemau.AccessKey{ID: "fake", Secret: "fake"}),
		nemau.WithRetry(nemau.RetryPolicy{MaxAttempts: 1}),
		nemau.WithPollInterval(time.Millisecond),
	)
	if err != nil {
		g.t.Fatalf("NewClient: %v", err)
	}
	return c
}

func (g *groupStub) sent() ([]string, []map[string]any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.requests...), append([]map[string]any(nil), g.patches...)
}

// storedGroup is the group as the platform holds it before a test changes it.
func storedGroup() nemau.InstanceGroup {
	return nemau.InstanceGroup{
		ID: "g-1", Name: "web", Status: "ok", HealthCheck: "instance",
		HealthCheckGraceSeconds: 300, DrainSeconds: 30, MinSize: 1, MaxSize: 5,
		DesiredCapacity: 4, InstanceCount: 4, MaxSurge: 1, MaxUnavailablePct: 0, TemplateVersion: 1,
		LaunchTemplate: nemau.LaunchTemplateInfo{
			ImageID: "debian-12", NetworkID: "net-1", SubnetID: "sub-1",
			SecurityGroupIDs: []string{}, UserDataPresent: false,
		},
	}
}

// groupModelOf is the model of a group whose configuration matches storedGroup,
// with the given changes applied.
func groupModelOf(t *testing.T, mutate func(*instanceGroupModel)) instanceGroupModel {
	t.Helper()
	ctx := context.Background()
	tmpl, d := types.ObjectValueFrom(ctx, launchTemplateAttrTypes, launchTemplateModel{
		ImageID: types.StringValue("debian-12"), FlavorID: types.StringNull(), VCPUs: types.Int64Null(),
		RAMMB: types.Int64Null(), DiskGB: types.Int64Null(), KeyName: types.StringNull(),
		ServiceLevel: types.StringNull(), UserData: types.StringNull(),
	})
	if d.HasError() {
		t.Fatal(d)
	}
	m := instanceGroupModel{
		ID: types.StringValue("g-1"), Name: types.StringValue("web"),
		NetworkID: types.StringValue("net-1"), SubnetID: types.StringValue("sub-1"),
		MinSize: types.Int64Value(1), MaxSize: types.Int64Value(5),
		DesiredCapacity:         types.Int64Value(4),
		SecurityGroups:          types.ListNull(types.StringType),
		HealthCheck:             types.StringValue("instance"),
		HealthCheckGraceSeconds: types.Int64Value(300), DrainSeconds: types.Int64Value(30),
		MaxUnavailablePct: types.Int64Value(0), MaxSurge: types.Int64Value(1),
		LoadBalancerPools: types.ListNull(types.ObjectType{AttrTypes: groupPoolAttrTypes}),
		ScalingPolicies:   types.ListNull(types.ObjectType{AttrTypes: groupPolicyAttrTypes}),
		ScheduledActions:  types.ListNull(types.ObjectType{AttrTypes: groupScheduleAttrTypes}),
		LaunchTemplate:    tmpl, DeleteInstancesOnDelete: types.BoolNull(),
		Status: types.StringValue("ok"), InstanceCount: types.Int64Value(4), TemplateVersion: types.Int64Value(1),
	}
	if mutate != nil {
		mutate(&m)
	}
	return m
}

// schedulesOf builds a scheduled_action list from crons, each setting min_size.
func schedulesOf(t *testing.T, crons ...string) types.List {
	t.Helper()
	items := make([]groupScheduleModel, 0, len(crons))
	for _, c := range crons {
		items = append(items, groupScheduleModel{
			Cron: cronValue{StringValue: types.StringValue(c)}, MinSize: types.Int64Value(2),
			MaxSize: types.Int64Null(), DesiredCapacity: types.Int64Null(),
		})
	}
	list, d := types.ListValueFrom(context.Background(), types.ObjectType{AttrTypes: groupScheduleAttrTypes}, items)
	if d.HasError() {
		t.Fatal(d)
	}
	return list
}

// fakePrivate stands in for the framework's private state: a map of keys.
type fakePrivate map[string][]byte

func (f fakePrivate) GetKey(_ context.Context, key string) ([]byte, diag.Diagnostics) {
	return f[key], nil
}

func (f fakePrivate) SetKey(_ context.Context, key string, value []byte) diag.Diagnostics {
	f[key] = value
	return nil
}

// groupConfig is the configuration of a model: an unknown computed value is
// one the configuration leaves unset, so it is null here.
func groupConfig(t *testing.T, m instanceGroupModel) tfsdk.Config {
	t.Helper()
	ctx := context.Background()
	if m.DesiredCapacity.IsUnknown() {
		m.DesiredCapacity = types.Int64Null()
	}
	var s resource.SchemaResponse
	NewInstanceGroupResource().Schema(ctx, resource.SchemaRequest{}, &s)
	plan := tfsdk.Plan{Schema: s.Schema}
	if d := plan.Set(ctx, &m); d.HasError() {
		t.Fatal(d)
	}
	return tfsdk.Config{Schema: s.Schema, Raw: plan.Raw}
}

// runGroupUpdate applies an update from prior to planned, with the configuration
// of planned and no private state. It returns the diagnostics as text.
func runGroupUpdate(t *testing.T, stub *groupStub, prior, planned instanceGroupModel) string {
	t.Helper()
	_, diags := runGroupUpdateWith(t, stub, prior, planned, groupConfigModel(planned), fakePrivate{})
	return diags
}

// groupConfigModel is the configuration a plan was made from.
func groupConfigModel(planned instanceGroupModel) instanceGroupModel {
	if planned.DesiredCapacity.IsUnknown() {
		planned.DesiredCapacity = types.Int64Null()
	}
	return planned
}

// runGroupUpdateWith applies an update from prior to planned under the given
// configuration and private state. It returns the resulting state and the
// diagnostics as text.
func runGroupUpdateWith(t *testing.T, stub *groupStub, prior, planned, cfg instanceGroupModel, private fakePrivate) (instanceGroupModel, string) {
	t.Helper()
	ctx := context.Background()
	r := NewInstanceGroupResource().(*instanceGroupResource)
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
	r.update(ctx, resource.UpdateRequest{
		Plan:   plan,
		State:  state,
		Config: groupConfig(t, cfg),
	}, &resp, private, private)
	var got instanceGroupModel
	if d := resp.State.Get(ctx, &got); d.HasError() {
		t.Fatal(d)
	}
	return got, diagText(resp.Diagnostics)
}

func diagText(ds diag.Diagnostics) string {
	messages := make([]string, 0, len(ds))
	for _, d := range ds {
		messages = append(messages, d.Summary()+": "+d.Detail())
	}
	return strings.Join(messages, "\n")
}

// The bounds exclude the desired capacity: the capacity moves first, to the
// nearest bound, and only then the PATCH sets the bounds.
func TestGroupUpdateMovesDesiredCapacityBeforeNewBounds(t *testing.T) {
	stub := &groupStub{t: t, group: storedGroup()}
	prior := groupModelOf(t, nil)
	planned := groupModelOf(t, func(m *instanceGroupModel) {
		m.MaxSize = types.Int64Value(3)
		m.DesiredCapacity = types.Int64Unknown()
	})
	if diags := runGroupUpdate(t, stub, prior, planned); diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	requests, patches := stub.sent()
	capacity, patch := indexOf(requests, http.MethodPut+" "+groupPath+"/capacity"), indexOf(requests, http.MethodPatch+" "+groupPath)
	if capacity < 0 || patch < 0 || capacity > patch {
		t.Fatalf("requests = %v, want the capacity PUT before the bounds PATCH", requests)
	}
	if len(patches) != 1 || patches[0]["max_size"] != float64(3) || patches[0]["min_size"] != float64(1) {
		t.Fatalf("PATCH bodies = %v, want the new bounds", patches)
	}
	if stub.group.DesiredCapacity != 3 {
		t.Errorf("desired capacity = %d, want it clamped to the new max 3", stub.group.DesiredCapacity)
	}
}

// A change of one field sends that field, and nothing else: no scaling policy,
// schedule, pool or sizing the state already holds goes back to the API.
func TestGroupUpdateSendsOnlyTheChangedFields(t *testing.T) {
	stub := &groupStub{t: t, group: storedGroup()}
	prior := groupModelOf(t, nil)
	planned := groupModelOf(t, func(m *instanceGroupModel) {
		m.HealthCheckGraceSeconds = types.Int64Value(400)
		m.DesiredCapacity = types.Int64Value(4)
	})
	// The last apply set 4 too: the configured capacity is unchanged, so no SetCapacity is sent.
	if _, diags := runGroupUpdateWith(t, stub, prior, planned, planned, fakePrivate{desiredConfigKey: []byte("4")}); diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	requests, patches := stub.sent()
	if indexOf(requests, http.MethodPut+" "+groupPath+"/capacity") >= 0 {
		t.Fatalf("requests = %v: SetCapacity was sent although the configured capacity did not change", requests)
	}
	if len(patches) != 1 {
		t.Fatalf("PATCH bodies = %v, want one", patches)
	}
	for key := range patches[0] {
		if key != "health_check_grace_seconds" {
			t.Errorf("unchanged field %q was sent: %v", key, patches[0])
		}
	}
	if patches[0]["health_check_grace_seconds"] != float64(400) {
		t.Errorf("grace = %v, want 400", patches[0]["health_check_grace_seconds"])
	}
}

// The platform checks a capacity against the current bounds. A group at 0/0
// moved to 2..5 has no overlap: the bounds are widened to the union first, the
// capacity is set, and only then do the final bounds apply.
func TestGroupUpdateWidensBoundsWhenTheNewRangeDoesNotOverlap(t *testing.T) {
	empty := storedGroup()
	empty.MinSize, empty.MaxSize, empty.DesiredCapacity, empty.InstanceCount = 0, 0, 0, 0
	stub := &groupStub{t: t, group: empty}
	prior := groupModelOf(t, func(m *instanceGroupModel) {
		m.MinSize, m.MaxSize, m.DesiredCapacity, m.InstanceCount = types.Int64Value(0), types.Int64Value(0), types.Int64Value(0), types.Int64Value(0)
	})
	planned := groupModelOf(t, func(m *instanceGroupModel) {
		m.MinSize, m.MaxSize = types.Int64Value(2), types.Int64Value(5)
		m.DesiredCapacity = types.Int64Unknown()
	})
	if diags := runGroupUpdate(t, stub, prior, planned); diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	requests, patches := stub.sent()
	if len(patches) != 2 {
		t.Fatalf("PATCH bodies = %v, want the widening then the final bounds", patches)
	}
	if patches[0]["min_size"] != float64(0) || patches[0]["max_size"] != float64(5) {
		t.Errorf("widening PATCH = %v, want the union 0..5", patches[0])
	}
	if patches[1]["min_size"] != float64(2) || patches[1]["max_size"] != float64(5) {
		t.Errorf("final PATCH = %v, want 2..5", patches[1])
	}
	first := indexOf(requests, http.MethodPatch+" "+groupPath)
	capacity := indexOf(requests, http.MethodPut+" "+groupPath+"/capacity")
	last := len(requests) - 1 - indexOf(reversed(requests), http.MethodPatch+" "+groupPath)
	if first < 0 || capacity < first || last < capacity {
		t.Errorf("requests = %v, want widen, capacity, then final bounds", requests)
	}
	if stub.group.DesiredCapacity != 2 || stub.group.MinSize != 2 || stub.group.MaxSize != 5 {
		t.Errorf("group = %d in %d..%d, want 2 in 2..5", stub.group.DesiredCapacity, stub.group.MinSize, stub.group.MaxSize)
	}
}

// A capacity the platform refuses after the bounds were widened leaves no
// widened bounds behind: the original bounds are PATCHed back, and the error
// returned is the capacity's.
func TestGroupUpdateRestoresTheBoundsWhenTheCapacityIsRefused(t *testing.T) {
	empty := storedGroup()
	empty.MinSize, empty.MaxSize, empty.DesiredCapacity, empty.InstanceCount = 0, 0, 0, 0
	stub := &groupStub{t: t, group: empty, rejectCapacity: true}
	prior := groupModelOf(t, func(m *instanceGroupModel) {
		m.MinSize, m.MaxSize, m.DesiredCapacity, m.InstanceCount = types.Int64Value(0), types.Int64Value(0), types.Int64Value(0), types.Int64Value(0)
	})
	planned := groupModelOf(t, func(m *instanceGroupModel) {
		m.MinSize, m.MaxSize = types.Int64Value(2), types.Int64Value(5)
		m.DesiredCapacity = types.Int64Unknown()
	})
	diags := runGroupUpdate(t, stub, prior, planned)
	if !strings.Contains(diags, "Setting the desired capacity within the new bounds") {
		t.Fatalf("diagnostics = %q, want the capacity error", diags)
	}
	_, patches := stub.sent()
	if len(patches) != 2 {
		t.Fatalf("PATCH bodies = %v, want the widening then the restore", patches)
	}
	if patches[0]["max_size"] != float64(5) {
		t.Errorf("widening PATCH = %v, want the union 0..5", patches[0])
	}
	if patches[1]["min_size"] != float64(0) || patches[1]["max_size"] != float64(0) {
		t.Errorf("restore PATCH = %v, want the original bounds 0..0", patches[1])
	}
	if stub.group.MinSize != 0 || stub.group.MaxSize != 0 {
		t.Errorf("bounds = %d..%d after the refusal, want the original 0..0", stub.group.MinSize, stub.group.MaxSize)
	}
}

func reversed(items []string) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[len(items)-1-i] = item
	}
	return out
}

// Two spellings of one cron are one schedule: a whitespace-only change sends
// nothing, and a real change sends the schedules normalized.
func TestGroupCronWhitespaceIsNotAChange(t *testing.T) {
	stored := storedGroup()
	stored.Schedules = []nemau.Schedule{{ID: "s-1", Cron: "0 6 * * 1-5", MinSize: intPtr(2)}}

	stub := &groupStub{t: t, group: stored}
	prior := groupModelOf(t, func(m *instanceGroupModel) {
		m.ScheduledActions = schedulesOf(t, "0 6 * * 1-5")
	})
	spaced := groupModelOf(t, func(m *instanceGroupModel) {
		m.ScheduledActions = schedulesOf(t, "  0  6 *  * 1-5 ")
	})
	if diags := runGroupUpdate(t, stub, prior, spaced); diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	if _, patches := stub.sent(); len(patches) != 0 {
		t.Fatalf("a whitespace-only cron change was sent: %v", patches)
	}

	changed := groupModelOf(t, func(m *instanceGroupModel) {
		m.ScheduledActions = schedulesOf(t, "0  7 * * 1-5 ")
	})
	if diags := runGroupUpdate(t, stub, prior, changed); diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	_, patches := stub.sent()
	if len(patches) != 1 {
		t.Fatalf("PATCH bodies = %v, want one", patches)
	}
	schedules, ok := patches[0]["schedules"].([]any)
	if !ok || len(schedules) != 1 || schedules[0].(map[string]any)["cron"] != "0 7 * * 1-5" {
		t.Errorf("schedules = %v, want the cron sent normalized", patches[0]["schedules"])
	}
}

// A read keeps the spelling the configuration uses, when it means the same
// schedule as the platform's.
func TestGroupReadKeepsTheWrittenCron(t *testing.T) {
	ctx := context.Background()
	var d diag.Diagnostics
	prior := schedulesOf(t, "0  6 * * 1-5")
	got := listFromSchedules(ctx, []nemau.Schedule{{Cron: "0 6 * * 1-5"}, {Cron: "0 9 * * *"}}, prior, &d)
	if d.HasError() {
		t.Fatal(d)
	}
	var items []groupScheduleModel
	if d := got.ElementsAs(ctx, &items, false); d.HasError() {
		t.Fatal(d)
	}
	if len(items) != 2 || items[0].Cron.ValueString() != "0  6 * * 1-5" || items[1].Cron.ValueString() != "0 9 * * *" {
		t.Errorf("crons = %v, want the written spelling kept and the other read as the API has it", items)
	}
}

// The configuration is refused when a cron is not five fields of allowed characters.
func TestGroupCronRejectsMalformedSchedules(t *testing.T) {
	ctx := context.Background()
	for _, cron := range []string{"0 6 * * 1-5 2026", "0 6 * * MON;", "", "0 6 * *"} {
		var resp resource.ValidateConfigResponse
		validateGroupBlocks(ctx, instanceGroupModel{
			LoadBalancerPools: types.ListNull(types.ObjectType{AttrTypes: groupPoolAttrTypes}),
			ScalingPolicies:   types.ListNull(types.ObjectType{AttrTypes: groupPolicyAttrTypes}),
			ScheduledActions:  schedulesOf(t, cron),
		}, &resp)
		if !resp.Diagnostics.HasError() {
			t.Errorf("cron %q accepted", cron)
		}
	}
}

func indexOf(items []string, want string) int {
	for i, item := range items {
		if item == want {
			return i
		}
	}
	return -1
}

func intPtr(v int) *int { return &v }

// The configured desired capacity is applied when its value changes, and only
// then: a refresh or an apply that leaves it alone never resends it.
func TestDesiredCapacityActionAppliesOnlyAConfigChange(t *testing.T) {
	set := func(v int64) types.Int64 { return types.Int64Value(v) }
	cases := []struct {
		label     string
		prior     string
		cfg       types.Int64
		wantSend  bool
		wantStore string
	}{
		{"first apply with a value", "", set(3), true, "3"},
		{"same value as the last apply", "3", set(3), false, "3"},
		{"value changed", "3", set(4), true, "4"},
		{"unset after a value", "3", types.Int64Null(), false, ""},
		{"unset, nothing before", "", types.Int64Null(), false, ""},
		{"unknown in the plan", "3", types.Int64Unknown(), false, ""},
	}
	for _, tc := range cases {
		send, now := desiredCapacityAction(tc.prior, tc.cfg)
		if send != tc.wantSend || now != tc.wantStore {
			t.Errorf("%s: send=%v store=%q, want send=%v store=%q", tc.label, send, now, tc.wantSend, tc.wantStore)
		}
	}
}

// A refresh with a configured desired capacity keeps the value applied, while
// the autoscaler has moved the group; an unset one takes the platform's value.
func TestGroupReadDoesNotUndoTheAutoscaler(t *testing.T) {
	ctx := context.Background()
	view := storedGroup()
	view.DesiredCapacity = 5

	kept := groupModelOf(t, nil) // desired_capacity 4 in the state
	kept.DesiredCapacity = types.Int64Value(3)
	if d := fillGroup(ctx, &kept, &view, true); d.HasError() {
		t.Fatal(d)
	}
	if kept.DesiredCapacity.ValueInt64() != 3 {
		t.Errorf("configured desired capacity = %v, want the applied 3", kept.DesiredCapacity)
	}

	unset := groupModelOf(t, nil)
	unset.DesiredCapacity = types.Int64Unknown()
	if d := fillGroup(ctx, &unset, &view, false); d.HasError() {
		t.Fatal(d)
	}
	if unset.DesiredCapacity.ValueInt64() != 5 {
		t.Errorf("unset desired capacity = %v, want the platform's 5", unset.DesiredCapacity)
	}
}

// The private state holds the capacity the last apply set. While the
// configuration keeps it, the autoscaler's move is not undone, and the state
// keeps the applied value: a wrong keep flag would show the platform's 5 here.
func TestGroupUpdateKeepsTheAppliedDesiredCapacityWhileTheAutoscalerMovesIt(t *testing.T) {
	moved := storedGroup()
	moved.DesiredCapacity = 5
	stub := &groupStub{t: t, group: moved}
	prior := groupModelOf(t, func(m *instanceGroupModel) { m.DesiredCapacity = types.Int64Value(3) })
	planned := groupModelOf(t, func(m *instanceGroupModel) {
		m.DesiredCapacity = types.Int64Value(3)
		m.HealthCheckGraceSeconds = types.Int64Value(400)
	})
	private := fakePrivate{desiredConfigKey: []byte("3")}
	got, diags := runGroupUpdateWith(t, stub, prior, planned, planned, private)
	if diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	requests, _ := stub.sent()
	if indexOf(requests, http.MethodPut+" "+groupPath+"/capacity") >= 0 {
		t.Errorf("requests = %v: the applied capacity was resent against the autoscaler", requests)
	}
	if got.DesiredCapacity.ValueInt64() != 3 {
		t.Errorf("state desired_capacity = %v, want the applied 3", got.DesiredCapacity)
	}
	if string(private[desiredConfigKey]) != "3" {
		t.Errorf("private value = %q, want 3", private[desiredConfigKey])
	}
}

// A configured capacity that differs from the one last applied is sent, and the
// private state then holds the new value.
func TestGroupUpdateSendsAConfiguredCapacityChangedSinceTheLastApply(t *testing.T) {
	stub := &groupStub{t: t, group: storedGroup()}
	prior := groupModelOf(t, func(m *instanceGroupModel) { m.DesiredCapacity = types.Int64Value(3) })
	planned := groupModelOf(t, func(m *instanceGroupModel) { m.DesiredCapacity = types.Int64Value(4) })
	private := fakePrivate{desiredConfigKey: []byte("3")}
	got, diags := runGroupUpdateWith(t, stub, prior, planned, planned, private)
	if diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	if stub.group.DesiredCapacity != 4 || got.DesiredCapacity.ValueInt64() != 4 {
		t.Errorf("desired capacity = platform %d, state %v; want 4", stub.group.DesiredCapacity, got.DesiredCapacity)
	}
	if string(private[desiredConfigKey]) != "4" {
		t.Errorf("private value = %q, want 4", private[desiredConfigKey])
	}
}

// Removing desired_capacity from the configuration clears the stored value: the
// state then takes the platform's value, and the autoscaler owns it again.
func TestGroupUpdateUnsetDesiredCapacityClearsTheStoredValue(t *testing.T) {
	moved := storedGroup()
	moved.DesiredCapacity = 5
	stub := &groupStub{t: t, group: moved}
	prior := groupModelOf(t, func(m *instanceGroupModel) { m.DesiredCapacity = types.Int64Value(3) })
	planned := groupModelOf(t, func(m *instanceGroupModel) {
		m.DesiredCapacity = types.Int64Unknown()
		m.HealthCheckGraceSeconds = types.Int64Value(400)
	})
	private := fakePrivate{desiredConfigKey: []byte("3")}
	got, diags := runGroupUpdateWith(t, stub, prior, planned, planned, private)
	if diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	requests, _ := stub.sent()
	if indexOf(requests, http.MethodPut+" "+groupPath+"/capacity") >= 0 {
		t.Errorf("requests = %v: an unset capacity was sent", requests)
	}
	if string(private[desiredConfigKey]) != "" {
		t.Errorf("private value = %q, want it cleared", private[desiredConfigKey])
	}
	if got.DesiredCapacity.ValueInt64() != 5 {
		t.Errorf("state desired_capacity = %v, want the platform's 5", got.DesiredCapacity)
	}
}

// runGroupRead reads the group into state under the given private state.
func runGroupRead(t *testing.T, stub *groupStub, state instanceGroupModel, private fakePrivate) instanceGroupModel {
	t.Helper()
	ctx := context.Background()
	r := NewInstanceGroupResource().(*instanceGroupResource)
	r.client = stub.client()
	var s resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &s)
	st := tfsdk.State{Schema: s.Schema}
	if d := st.Set(ctx, &state); d.HasError() {
		t.Fatal(d)
	}
	resp := resource.ReadResponse{State: st}
	r.read(ctx, resource.ReadRequest{State: st}, &resp, private)
	if d := resp.Diagnostics; d.HasError() {
		t.Fatal(d)
	}
	var got instanceGroupModel
	if d := resp.State.Get(ctx, &got); d.HasError() {
		t.Fatal(d)
	}
	return got
}

// A read keeps the applied capacity while the configuration sets it, and takes
// the platform's value once the stored value is cleared.
func TestGroupReadKeepsTheAppliedDesiredCapacityOnlyWhileConfigured(t *testing.T) {
	moved := storedGroup()
	moved.DesiredCapacity = 5
	stub := &groupStub{t: t, group: moved}
	state := groupModelOf(t, func(m *instanceGroupModel) { m.DesiredCapacity = types.Int64Value(3) })

	if got := runGroupRead(t, stub, state, fakePrivate{desiredConfigKey: []byte("3")}); got.DesiredCapacity.ValueInt64() != 3 {
		t.Errorf("configured read = %v, want the applied 3", got.DesiredCapacity)
	}
	if got := runGroupRead(t, stub, state, fakePrivate{desiredConfigKey: []byte("")}); got.DesiredCapacity.ValueInt64() != 5 {
		t.Errorf("unconfigured read = %v, want the platform's 5", got.DesiredCapacity)
	}
}

// planGroupWith plans a group update: the prior state, the planned values, the
// configuration and the prior private state. It returns the planned model.
func planGroupWith(t *testing.T, prior, planned, cfg instanceGroupModel, private fakePrivate) (instanceGroupModel, string) {
	t.Helper()
	ctx := context.Background()
	r := NewInstanceGroupResource().(*instanceGroupResource)
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
	resp := resource.ModifyPlanResponse{Plan: plan}
	r.modifyPlan(ctx, resource.ModifyPlanRequest{Plan: plan, State: state, Config: groupConfig(t, cfg)}, &resp, private)
	var got instanceGroupModel
	if d := resp.Plan.Get(ctx, &got); d.HasError() {
		t.Fatal(d)
	}
	return got, diagText(resp.Diagnostics)
}

// A desired capacity the configuration no longer sets is planned as unknown, so
// the plan shows the platform's value; a configured or unstored one keeps the
// planned value.
func TestGroupPlanShowsAnUnsetDesiredCapacityAsUnknown(t *testing.T) {
	prior := groupModelOf(t, func(m *instanceGroupModel) { m.DesiredCapacity = types.Int64Value(3) })
	planned := groupModelOf(t, func(m *instanceGroupModel) { m.DesiredCapacity = types.Int64Value(3) })
	unset := groupModelOf(t, func(m *instanceGroupModel) { m.DesiredCapacity = types.Int64Null() })
	configured := groupModelOf(t, func(m *instanceGroupModel) { m.DesiredCapacity = types.Int64Value(3) })

	got, diags := planGroupWith(t, prior, planned, unset, fakePrivate{desiredConfigKey: []byte("3")})
	if diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	if !got.DesiredCapacity.IsUnknown() {
		t.Errorf("planned desired_capacity = %v, want unknown once the configuration unsets it", got.DesiredCapacity)
	}

	got, _ = planGroupWith(t, prior, planned, configured, fakePrivate{desiredConfigKey: []byte("3")})
	if got.DesiredCapacity.IsUnknown() || got.DesiredCapacity.ValueInt64() != 3 {
		t.Errorf("configured desired_capacity planned as %v, want 3", got.DesiredCapacity)
	}

	got, _ = planGroupWith(t, prior, planned, unset, fakePrivate{})
	if got.DesiredCapacity.IsUnknown() || got.DesiredCapacity.ValueInt64() != 3 {
		t.Errorf("unset desired_capacity with nothing stored planned as %v, want the planned 3", got.DesiredCapacity)
	}
}

// A changed desired capacity is applied by an update of the group.
func TestGroupUpdateAppliesAChangedDesiredCapacity(t *testing.T) {
	stub := &groupStub{t: t, group: storedGroup()}
	prior := groupModelOf(t, nil)
	planned := groupModelOf(t, func(m *instanceGroupModel) {
		m.DesiredCapacity = types.Int64Value(3)
		m.InstanceCount = types.Int64Value(4)
	})
	if diags := runGroupUpdate(t, stub, prior, planned); diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	requests, _ := stub.sent()
	if indexOf(requests, http.MethodPut+" "+groupPath+"/capacity") < 0 {
		t.Fatalf("requests = %v, want the capacity set", requests)
	}
	if stub.group.DesiredCapacity != 3 {
		t.Errorf("desired capacity = %d, want 3", stub.group.DesiredCapacity)
	}
}

// With the desired capacity unset, an update leaves the autoscaler's value alone.
func TestGroupUpdateLeavesAnUnsetDesiredCapacityToTheAutoscaler(t *testing.T) {
	stub := &groupStub{t: t, group: storedGroup()}
	prior := groupModelOf(t, nil)
	planned := groupModelOf(t, func(m *instanceGroupModel) {
		m.HealthCheckGraceSeconds = types.Int64Value(400)
		m.DesiredCapacity = types.Int64Unknown()
	})
	if diags := runGroupUpdate(t, stub, prior, planned); diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	requests, _ := stub.sent()
	for _, r := range requests {
		if strings.HasPrefix(r, http.MethodPut) {
			t.Fatalf("requests = %v: the capacity was set although unset", requests)
		}
	}
}

// fixtureDocument stands in for a cloud-init document. It is never real.
const fixtureDocument = "#cloud-config\n# FIXTURE-DOCUMENT-NOT-REAL\n"

// withDocument sets the user_data of the model's template.
func withDocument(t *testing.T, m *instanceGroupModel, doc types.String) {
	t.Helper()
	ctx := context.Background()
	var tmpl launchTemplateModel
	if d := m.LaunchTemplate.As(ctx, &tmpl, basetypes.ObjectAsOptions{}); d.HasError() {
		t.Fatal(d)
	}
	tmpl.UserData = doc
	obj, d := types.ObjectValueFrom(ctx, launchTemplateAttrTypes, tmpl)
	if d.HasError() {
		t.Fatal(d)
	}
	m.LaunchTemplate = obj
}

func planGroup(t *testing.T, prior, planned instanceGroupModel) string {
	t.Helper()
	ctx := context.Background()
	r := NewInstanceGroupResource().(*instanceGroupResource)
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
	resp := resource.ModifyPlanResponse{Plan: plan}
	r.ModifyPlan(ctx, resource.ModifyPlanRequest{Plan: plan, State: state, Config: tfsdk.Config{Schema: s.Schema, Raw: plan.Raw}}, &resp)
	return diagText(resp.Diagnostics)
}

// Removing user_data from the configuration is refused at plan time, and the
// refusal does not echo the document.
func TestGroupPlanRefusesRemovingUserData(t *testing.T) {
	prior := groupModelOf(t, nil)
	withDocument(t, &prior, types.StringValue(fixtureDocument))
	planned := groupModelOf(t, nil)

	got := planGroup(t, prior, planned)
	if !strings.Contains(got, "user_data cannot be removed; set a new document or replace the group") {
		t.Fatalf("diagnostics = %q", got)
	}
	if strings.Contains(got, "FIXTURE-DOCUMENT") {
		t.Errorf("the refusal echoes the document: %q", got)
	}

	// Replacing the group is the way out: that plan is not refused.
	replaced := groupModelOf(t, func(m *instanceGroupModel) { m.Name = types.StringValue("web-2") })
	if got := planGroup(t, prior, replaced); got != "" {
		t.Errorf("a replacement was refused: %s", got)
	}
	// Keeping or changing the document is allowed, and so is a create.
	kept := groupModelOf(t, nil)
	withDocument(t, &kept, types.StringValue(fixtureDocument))
	if got := planGroup(t, prior, kept); got != "" {
		t.Errorf("an unchanged document was refused: %s", got)
	}
	if got := planGroup(t, prior, planGroupUnknownDocument(t)); got != "" {
		t.Errorf("an unknown document was refused: %s", got)
	}
}

func planGroupUnknownDocument(t *testing.T) instanceGroupModel {
	m := groupModelOf(t, nil)
	withDocument(t, &m, types.StringUnknown())
	return m
}

// After an import the state holds no document. A configured document whose
// digest the platform holds needs no new template version.
func TestGroupUpdateDoesNotRollTheTemplateForAMatchingDocument(t *testing.T) {
	stored := storedGroup()
	stored.LaunchTemplate.UserDataPresent = true
	stored.LaunchTemplate.UserDataSHA256 = userDataDigest(fixtureDocument)
	stub := &groupStub{t: t, group: stored}

	prior := groupModelOf(t, nil) // no user_data in the state, as after an import
	planned := groupModelOf(t, nil)
	withDocument(t, &planned, types.StringValue(fixtureDocument))
	if diags := runGroupUpdate(t, stub, prior, planned); diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	if _, patches := stub.sent(); len(patches) != 0 {
		t.Fatalf("the template was rolled although the digest matches: %v", patches)
	}
}

// A document the platform does not hold rolls the template, and the new version
// carries the configured document.
func TestGroupUpdateRollsTheTemplateForADifferentDocument(t *testing.T) {
	stored := storedGroup()
	stored.LaunchTemplate.UserDataPresent = true
	stored.LaunchTemplate.UserDataSHA256 = userDataDigest("#cloud-config\n# OLD\n")
	stub := &groupStub{t: t, group: stored}

	prior := groupModelOf(t, nil)
	withDocument(t, &prior, types.StringValue("#cloud-config\n# OLD\n"))
	planned := groupModelOf(t, nil)
	withDocument(t, &planned, types.StringValue(fixtureDocument))
	if diags := runGroupUpdate(t, stub, prior, planned); diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	_, patches := stub.sent()
	if len(patches) != 1 {
		t.Fatalf("PATCH bodies = %v, want one", patches)
	}
	tmpl, ok := patches[0]["launch_template"].(map[string]any)
	if !ok || tmpl["user_data"] != fixtureDocument {
		t.Errorf("launch_template = %v, want the new document", patches[0]["launch_template"])
	}
}

// A read whose digest differs from the state's document drops the document, so
// the next plan shows the configured one as a change; a matching digest keeps it.
func TestGroupReadDropsADocumentTheDigestDoesNotMatch(t *testing.T) {
	ctx := context.Background()
	m := groupModelOf(t, nil)
	withDocument(t, &m, types.StringValue(fixtureDocument))

	if d := reconcileUserData(ctx, &m, userDataDigest(fixtureDocument)); d.HasError() {
		t.Fatal(d)
	}
	if doc, ok, _ := configuredUserData(ctx, m.LaunchTemplate); !ok || doc != fixtureDocument {
		t.Fatalf("a matching digest dropped the document")
	}
	if d := reconcileUserData(ctx, &m, userDataDigest("#cloud-config\n# changed outside\n")); d.HasError() {
		t.Fatal(d)
	}
	if _, ok, _ := configuredUserData(ctx, m.LaunchTemplate); ok {
		t.Error("a different digest kept the document in state")
	}
}

// setTemplate edits the launch_template of the model.
func setTemplate(t *testing.T, m *instanceGroupModel, mutate func(*launchTemplateModel)) {
	t.Helper()
	ctx := context.Background()
	var tmpl launchTemplateModel
	if d := m.LaunchTemplate.As(ctx, &tmpl, basetypes.ObjectAsOptions{}); d.HasError() {
		t.Fatal(d)
	}
	mutate(&tmpl)
	obj, d := types.ObjectValueFrom(ctx, launchTemplateAttrTypes, tmpl)
	if d.HasError() {
		t.Fatal(d)
	}
	m.LaunchTemplate = obj
}

// groupFlavorFixture is a flavor id in the form the SDK checks. It is not real.
const groupFlavorFixture = "7d2e9a10-4b5c-4e8f-a1b2-c3d4e5f60718"

// A create whose plan leaves service_level unknown. The group does not return
// the service level, so the state must hold null: an unknown value after apply
// is an error.
func TestGroupCreateNeverStoresAnUnknownServiceLevel(t *testing.T) {
	ctx := context.Background()
	stub := &groupStub{t: t, group: storedGroup()}
	r := NewInstanceGroupResource().(*instanceGroupResource)
	r.client = stub.client()

	var s resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &s)
	planned := groupModelOf(t, func(m *instanceGroupModel) {
		m.ID = types.StringUnknown()
		m.Status = types.StringUnknown()
		m.InstanceCount = types.Int64Unknown()
		m.TemplateVersion = types.Int64Unknown()
		m.DesiredCapacity = types.Int64Unknown()
		m.NetworkID = types.StringValue("0b9a7c31-2d4e-4f60-8a1b-5c6d7e8f9a0b")
		m.SubnetID = types.StringValue("a4b5c6d7-e8f9-4a0b-9c1d-2e3f4a5b6c7d")
		setTemplate(t, m, func(tmpl *launchTemplateModel) {
			tmpl.FlavorID = types.StringValue(groupFlavorFixture)
			tmpl.ServiceLevel = types.StringUnknown()
		})
	})
	config := groupModelOf(t, func(m *instanceGroupModel) {
		m.DesiredCapacity = types.Int64Null()
		m.NetworkID = planned.NetworkID
		m.SubnetID = planned.SubnetID
		setTemplate(t, m, func(tmpl *launchTemplateModel) { tmpl.FlavorID = types.StringValue(groupFlavorFixture) })
	})

	plan := tfsdk.Plan{Schema: s.Schema}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatal(d)
	}
	configPlan := tfsdk.Plan{Schema: s.Schema}
	if d := configPlan.Set(ctx, &config); d.HasError() {
		t.Fatal(d)
	}
	cfg := tfsdk.Config{Schema: s.Schema, Raw: configPlan.Raw}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s.Schema, Raw: tftypes.NewValue(s.Schema.Type().TerraformType(ctx), nil)}}
	r.Create(ctx, resource.CreateRequest{Plan: plan, Config: cfg}, &resp)
	if diags := diagText(resp.Diagnostics); diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Fatalf("state holds an unknown value after create: %v", resp.State.Raw)
	}
	var got instanceGroupModel
	if d := resp.State.Get(ctx, &got); d.HasError() {
		t.Fatal(d)
	}
	var tmpl launchTemplateModel
	if d := got.LaunchTemplate.As(ctx, &tmpl, basetypes.ObjectAsOptions{}); d.HasError() {
		t.Fatal(d)
	}
	if !tmpl.ServiceLevel.IsNull() {
		t.Errorf("service_level = %v, want null: the view does not return it", tmpl.ServiceLevel)
	}
}

// A document that starts with a byte order mark keeps the platform's digest:
// the platform strips the mark before it hashes, so a mark the configuration
// adds must not read as a change of the template.
func TestUserDataWithAByteOrderMarkDoesNotDrift(t *testing.T) {
	ctx := context.Background()
	const bom = "\uFEFF"
	if userDataDigest(bom+fixtureDocument) != userDataDigest(fixtureDocument) {
		t.Fatal("a leading byte order mark changes the digest")
	}

	m := groupModelOf(t, nil)
	withDocument(t, &m, types.StringValue(bom+fixtureDocument))
	if d := reconcileUserData(ctx, &m, userDataDigest(fixtureDocument)); d.HasError() {
		t.Fatal(d)
	}
	if doc, ok, _ := configuredUserData(ctx, m.LaunchTemplate); !ok || doc != bom+fixtureDocument {
		t.Error("a document with a byte order mark was dropped although the platform holds it")
	}

	stored := storedGroup()
	stored.LaunchTemplate.UserDataPresent = true
	stored.LaunchTemplate.UserDataSHA256 = userDataDigest(fixtureDocument)
	stub := &groupStub{t: t, group: stored}
	prior := groupModelOf(t, nil)
	planned := groupModelOf(t, nil)
	withDocument(t, &planned, types.StringValue(bom+fixtureDocument))
	if diags := runGroupUpdate(t, stub, prior, planned); diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	if _, patches := stub.sent(); len(patches) != 0 {
		t.Fatalf("the template was rolled for a byte order mark alone: %v", patches)
	}
}

// After an import the state holds no service level, since the view does not
// return one, and the import marker is set. An unset configured service level
// rolls nothing. A configured one is sent once with the template, and the marker
// ends only with an update that succeeded.
func TestGroupServiceLevelAfterImportRollsOnceWhenConfigured(t *testing.T) {
	setLevel := func(v string) func(*instanceGroupModel) {
		return func(m *instanceGroupModel) {
			setTemplate(t, m, func(tmpl *launchTemplateModel) { tmpl.ServiceLevel = types.StringValue(v) })
		}
	}

	t.Run("unset in the configuration rolls nothing and keeps the marker", func(t *testing.T) {
		stub := &groupStub{t: t, group: storedGroup()}
		prior := groupModelOf(t, nil)
		planned := groupModelOf(t, nil)
		private := fakePrivate{serviceLevelUnknownKey: []byte("true")}
		if _, diags := runGroupUpdateWith(t, stub, prior, planned, planned, private); diags != "" {
			t.Fatalf("diagnostics: %s", diags)
		}
		if _, patches := stub.sent(); len(patches) != 0 {
			t.Fatalf("PATCH bodies = %v: an unset service level rolled the template", patches)
		}
		if string(private[serviceLevelUnknownKey]) != "true" {
			t.Errorf("the import marker = %q after an update that sent no service level, want it kept", private[serviceLevelUnknownKey])
		}
	})

	// The drift case: the imported group runs at 99.9%, the state holds none, and
	// the configuration leaves service_level unset. A min_size-only update sends
	// no level, so the marker must survive it. A later apply that configures
	// best-effort then differs from the unknown level, and its roll ends the marker.
	t.Run("a min_size-only update with the level unset keeps the marker until a configured roll", func(t *testing.T) {
		stub := &groupStub{t: t, group: storedGroup()}
		prior := groupModelOf(t, nil)
		private := fakePrivate{serviceLevelUnknownKey: []byte("true")}

		resized := groupModelOf(t, func(m *instanceGroupModel) { m.MinSize = types.Int64Value(2) })
		if _, diags := runGroupUpdateWith(t, stub, prior, resized, resized, private); diags != "" {
			t.Fatalf("diagnostics: %s", diags)
		}
		_, patches := stub.sent()
		if len(patches) == 0 {
			t.Fatal("the min_size change was not sent")
		}
		for _, p := range patches {
			if _, ok := p["launch_template"]; ok {
				t.Fatalf("PATCH bodies = %v: an unset service level rolled the template", patches)
			}
		}
		if string(private[serviceLevelUnknownKey]) != "true" {
			t.Fatalf("the import marker = %q after a min_size-only update, want it kept", private[serviceLevelUnknownKey])
		}

		configured := groupModelOf(t, func(m *instanceGroupModel) {
			m.MinSize = types.Int64Value(2)
			setTemplate(t, m, func(tmpl *launchTemplateModel) { tmpl.ServiceLevel = types.StringValue("best-effort") })
		})
		if _, diags := runGroupUpdateWith(t, stub, resized, configured, configured, private); diags != "" {
			t.Fatalf("diagnostics on the configured apply: %s", diags)
		}
		if len(private[serviceLevelUnknownKey]) != 0 {
			t.Errorf("the import marker survived the configured roll: %q", private[serviceLevelUnknownKey])
		}
	})

	t.Run("configured rolls the template once and clears the marker after success", func(t *testing.T) {
		stub := &groupStub{t: t, group: storedGroup()}
		prior := groupModelOf(t, nil)
		planned := groupModelOf(t, setLevel("99.9%"))
		private := fakePrivate{serviceLevelUnknownKey: []byte("true")}
		if _, diags := runGroupUpdateWith(t, stub, prior, planned, planned, private); diags != "" {
			t.Fatalf("diagnostics: %s", diags)
		}
		_, patches := stub.sent()
		if len(patches) != 1 {
			t.Fatalf("PATCH bodies = %v, want one template roll", patches)
		}
		tmpl, _ := patches[0]["launch_template"].(map[string]any)
		if tmpl["service_level"] != "99.9%" {
			t.Errorf("launch_template = %v, want service_level 99.9%%", patches[0]["launch_template"])
		}
		if len(private[serviceLevelUnknownKey]) != 0 {
			t.Errorf("the import marker survived the successful roll: %q", private[serviceLevelUnknownKey])
		}
	})

	t.Run("a failed update keeps the marker, and the next apply sends the roll", func(t *testing.T) {
		stub := &groupStub{t: t, group: storedGroup(), rejectPatch: true}
		prior := groupModelOf(t, nil)
		planned := groupModelOf(t, setLevel("99.9%"))
		private := fakePrivate{serviceLevelUnknownKey: []byte("true")}
		if _, diags := runGroupUpdateWith(t, stub, prior, planned, planned, private); diags == "" {
			t.Fatal("a refused PATCH reported no error")
		}
		if string(private[serviceLevelUnknownKey]) != "true" {
			t.Fatalf("the import marker = %q after a failed update, want it kept", private[serviceLevelUnknownKey])
		}

		stub.rejectPatch = false
		if _, diags := runGroupUpdateWith(t, stub, prior, planned, planned, private); diags != "" {
			t.Fatalf("diagnostics on retry: %s", diags)
		}
		if _, patches := stub.sent(); len(patches) != 1 {
			t.Fatalf("PATCH bodies after retry = %v, want one template roll", patches)
		}
		if len(private[serviceLevelUnknownKey]) != 0 {
			t.Errorf("the import marker survived the successful retry: %q", private[serviceLevelUnknownKey])
		}
	})
}

// A group created without service_level stores null, which is the default
// best-effort. A configuration that says best-effort describes the same group,
// so it rolls nothing.
func TestGroupCreatedUnsetServiceLevelIsNotRolledForBestEffort(t *testing.T) {
	setLevel := func(v string) func(*instanceGroupModel) {
		return func(m *instanceGroupModel) {
			setTemplate(t, m, func(tmpl *launchTemplateModel) { tmpl.ServiceLevel = types.StringValue(v) })
		}
	}
	stub := &groupStub{t: t, group: storedGroup()}
	prior := groupModelOf(t, nil)
	planned := groupModelOf(t, setLevel("best-effort"))
	if _, diags := runGroupUpdateWith(t, stub, prior, planned, planned, fakePrivate{}); diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	if _, patches := stub.sent(); len(patches) != 0 {
		t.Fatalf("PATCH bodies = %v: best-effort against a null state rolled the template", patches)
	}
}

// A group created without service_level stores null. A configured service level
// is then a change of the template: the update rolls it, and the marker of an
// import does not excuse it.
func TestGroupCreatedUnsetServiceLevelRollsForAConfiguredOne(t *testing.T) {
	setLevel := func(v string) func(*instanceGroupModel) {
		return func(m *instanceGroupModel) {
			setTemplate(t, m, func(tmpl *launchTemplateModel) { tmpl.ServiceLevel = types.StringValue(v) })
		}
	}
	stub := &groupStub{t: t, group: storedGroup()}
	prior := groupModelOf(t, nil)
	planned := groupModelOf(t, setLevel("99.9%"))
	if _, diags := runGroupUpdateWith(t, stub, prior, planned, planned, fakePrivate{}); diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	_, patches := stub.sent()
	if len(patches) != 1 {
		t.Fatalf("PATCH bodies = %v, want one template roll", patches)
	}
	tmpl, _ := patches[0]["launch_template"].(map[string]any)
	if tmpl["service_level"] != "99.9%" {
		t.Errorf("launch_template = %v, want service_level 99.9%%", patches[0]["launch_template"])
	}
}

// The import writes the marker that keeps a configured service level from
// being compared with the state, which holds none after an import.
func TestGroupImportMarksTheServiceLevelUnknown(t *testing.T) {
	ctx := context.Background()
	var s resource.SchemaResponse
	NewInstanceGroupResource().Schema(ctx, resource.SchemaRequest{}, &s)
	resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s.Schema, Raw: tftypes.NewValue(s.Schema.Type().TerraformType(ctx), nil)}}
	private := fakePrivate{}
	importGroup(ctx, resource.ImportStateRequest{ID: "g-1"}, &resp, private)
	if diags := diagText(resp.Diagnostics); diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	if string(private[serviceLevelUnknownKey]) != "true" {
		t.Errorf("import marker = %q, want true", private[serviceLevelUnknownKey])
	}
}

// The desired capacity the configuration sets on create is written to the
// private state, so that the next apply knows what the configuration set.
func TestGroupCreateWritesTheConfiguredDesiredCapacity(t *testing.T) {
	ctx := context.Background()
	stub := &groupStub{t: t, group: storedGroup()}
	r := NewInstanceGroupResource().(*instanceGroupResource)
	r.client = stub.client()

	var s resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &s)
	planned := groupModelOf(t, func(m *instanceGroupModel) { // desired_capacity 4
		m.NetworkID = types.StringValue("0b9a7c31-2d4e-4f60-8a1b-5c6d7e8f9a0b")
		m.SubnetID = types.StringValue("a4b5c6d7-e8f9-4a0b-9c1d-2e3f4a5b6c7d")
		setTemplate(t, m, func(tmpl *launchTemplateModel) { tmpl.FlavorID = types.StringValue(groupFlavorFixture) })
	})
	plan := tfsdk.Plan{Schema: s.Schema}
	if d := plan.Set(ctx, &planned); d.HasError() {
		t.Fatal(d)
	}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s.Schema, Raw: tftypes.NewValue(s.Schema.Type().TerraformType(ctx), nil)}}
	private := fakePrivate{}
	r.create(ctx, resource.CreateRequest{Plan: plan, Config: groupConfig(t, planned)}, &resp, private)
	if diags := diagText(resp.Diagnostics); diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	if string(private[desiredConfigKey]) != "4" {
		t.Errorf("private value = %q, want 4", private[desiredConfigKey])
	}
}

// A template change rolls the members at once only when the configuration asks
// for it; the default stores the new version and starts no refresh.
func TestGroupTemplateChangeStartsARefreshOnlyWhenAsked(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refresh types.Bool
		want    int
	}{
		{"flag true", types.BoolValue(true), 1},
		{"flag false", types.BoolValue(false), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &groupStub{t: t, group: storedGroup()}
			prior := groupModelOf(t, nil)
			planned := groupModelOf(t, func(m *instanceGroupModel) {
				setTemplate(t, m, func(tmpl *launchTemplateModel) { tmpl.ImageID = types.StringValue("debian-13") })
				m.RefreshOnTemplateChange = tc.refresh
			})
			if diags := runGroupUpdate(t, stub, prior, planned); diags != "" {
				t.Fatalf("diagnostics: %s", diags)
			}
			if _, patches := stub.sent(); len(patches) != 1 {
				t.Fatalf("PATCH bodies = %v, want the template roll", patches)
			}
			if stub.refreshes != tc.want {
				t.Errorf("refresh requests = %d, want %d", stub.refreshes, tc.want)
			}
		})
	}
}

// Changing only the refresh flag rolls nothing and starts nothing.
func TestGroupRefreshFlagAloneStartsNothing(t *testing.T) {
	stub := &groupStub{t: t, group: storedGroup()}
	prior := groupModelOf(t, func(m *instanceGroupModel) { m.RefreshOnTemplateChange = types.BoolValue(false) })
	planned := groupModelOf(t, func(m *instanceGroupModel) { m.RefreshOnTemplateChange = types.BoolValue(true) })
	if diags := runGroupUpdate(t, stub, prior, planned); diags != "" {
		t.Fatalf("diagnostics: %s", diags)
	}
	if _, patches := stub.sent(); len(patches) != 0 || stub.refreshes != 0 {
		t.Errorf("patches = %d, refreshes = %d; want none", len(patches), stub.refreshes)
	}
}
