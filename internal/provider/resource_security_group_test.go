package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

// planSecurityGroup runs the security group's ModifyPlan. prior is nil on a
// create; configured is nil when the attribute is omitted.
func planSecurityGroup(t *testing.T, prior, configured *bool) string {
	t.Helper()
	ctx := context.Background()
	r := NewSecurityGroupResource().(resource.ResourceWithModifyPlan)
	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	objectType := schemaResp.Schema.Type().TerraformType(ctx).(tftypes.Object)

	boolValue := func(b *bool) tftypes.Value {
		if b == nil {
			return tftypes.NewValue(tftypes.Bool, nil)
		}
		return tftypes.NewValue(tftypes.Bool, *b)
	}
	object := func(id tftypes.Value, egress tftypes.Value) tftypes.Value {
		return tftypes.NewValue(objectType, map[string]tftypes.Value{
			"id":               id,
			"name":             tftypes.NewValue(tftypes.String, "web"),
			"description":      tftypes.NewValue(tftypes.String, nil),
			"egress_filtering": egress,
		})
	}
	config := object(tftypes.NewValue(tftypes.String, nil), boolValue(configured))
	planned := boolValue(configured)
	state := tftypes.NewValue(objectType, nil)
	id := tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	if prior != nil {
		id = tftypes.NewValue(tftypes.String, "sg-1")
		state = object(id, boolValue(prior))
		if configured == nil {
			planned = boolValue(prior) // what UseStateForUnknown leaves
		}
	} else if configured == nil {
		planned = tftypes.NewValue(tftypes.Bool, tftypes.UnknownValue)
	}
	plan := object(id, planned)

	req := resource.ModifyPlanRequest{
		Config: tfsdk.Config{Schema: schemaResp.Schema, Raw: config},
		Plan:   tfsdk.Plan{Schema: schemaResp.Schema, Raw: plan},
		State:  tfsdk.State{Schema: schemaResp.Schema, Raw: state},
	}
	resp := resource.ModifyPlanResponse{Plan: req.Plan}
	r.ModifyPlan(ctx, req, &resp)
	messages := make([]string, 0, len(resp.Diagnostics))
	for _, d := range resp.Diagnostics {
		messages = append(messages, d.Summary()+": "+d.Detail())
	}
	return strings.Join(messages, "\n")
}

func TestEgressFilteringOnlyGoesOneWay(t *testing.T) {
	on, off := true, false
	cases := []struct {
		label             string
		prior, configured *bool
		want              string // "" = allowed
	}{
		{"create, omitted", nil, nil, ""},
		{"create, true", nil, &on, ""},
		{"create, false", nil, &off, "A new security group always filters egress"},
		{"old group, omitted", &off, nil, ""},
		{"old group kept as is", &off, &off, ""},
		{"old group converted", &off, &on, ""},
		{"filtering group, omitted", &on, nil, ""},
		{"filtering group turned off", &on, &off, "Egress filtering cannot be turned off"},
	}
	for _, tc := range cases {
		got := planSecurityGroup(t, tc.prior, tc.configured)
		if tc.want == "" && got != "" {
			t.Errorf("%s: unexpected diagnostics: %s", tc.label, got)
		}
		if tc.want != "" && !strings.Contains(got, tc.want) {
			t.Errorf("%s: diagnostics %q, want %q", tc.label, got, tc.want)
		}
	}
}

// Omitted, the flag must keep what the API reported instead of planning an
// unknown on every unrelated change.
func TestEgressFilteringIsOptionalComputedAndKeepsState(t *testing.T) {
	ctx := context.Background()
	var resp resource.SchemaResponse
	NewSecurityGroupResource().Schema(ctx, resource.SchemaRequest{}, &resp)
	attr, ok := resp.Schema.Attributes["egress_filtering"].(fwresource.BoolAttribute)
	if !ok {
		t.Fatal("egress_filtering missing")
	}
	if !attr.Optional || !attr.Computed {
		t.Fatal("egress_filtering must be Optional and Computed")
	}
	keepsState := false
	for _, modifier := range attr.PlanModifiers {
		keepsState = keepsState || strings.Contains(strings.ToLower(modifier.Description(ctx)), "will not change")
	}
	if !keepsState {
		t.Fatal("egress_filtering must keep its state when omitted")
	}
}

// Turning egress_filtering from false to true runs the conversion, and only
// the conversion: an unchanged name and description send no PATCH.
func TestEgressFilteringFalseToTrueConverts(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/network/security-groups/sg-1/enable-egress-filtering":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task_id":"t-1","status":"pending"}`))
		case "GET /api/v1/tasks/t-1":
			_, _ = w.Write([]byte(`{"id":"t-1","status":"completed","progress":100,"resourceId":"sg-1"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	client, err := nemau.NewClient(nemau.WithBaseURL(srv.URL),
		nemau.WithCredential(nemau.AccessKey{ID: "AKIA", Secret: "s3cr3t"}),
		nemau.WithPollInterval(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}

	r := &securityGroupResource{client: client}
	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	objectType := schemaResp.Schema.Type().TerraformType(ctx).(tftypes.Object)
	object := func(egress bool) tftypes.Value {
		return tftypes.NewValue(objectType, map[string]tftypes.Value{
			"id":               tftypes.NewValue(tftypes.String, "sg-1"),
			"name":             tftypes.NewValue(tftypes.String, "legacy"),
			"description":      tftypes.NewValue(tftypes.String, nil),
			"egress_filtering": tftypes.NewValue(tftypes.Bool, egress),
		})
	}
	req := resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: schemaResp.Schema, Raw: object(true)},
		State: tfsdk.State{Schema: schemaResp.Schema, Raw: object(false)},
	}
	resp := resource.UpdateResponse{State: tfsdk.State{Schema: schemaResp.Schema, Raw: object(false)}}
	r.Update(ctx, req, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	var state securityGroupModel
	resp.Diagnostics.Append(resp.State.Get(ctx, &state)...)
	if !state.EgressFiltering.ValueBool() {
		t.Fatal("the state must record the conversion")
	}
	if len(calls) == 0 || calls[0] != "POST /api/v1/network/security-groups/sg-1/enable-egress-filtering" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestRuleImportIDNamesTheGroupAndTheRule(t *testing.T) {
	for _, tc := range []struct {
		id, group, rule string
		ok              bool
	}{
		{"sg-1:rule-1", "sg-1", "rule-1", true},
		{" sg-1:rule-1 ", "sg-1", "rule-1", true},
		{"rule-1", "", "", false},
		{"sg-1:", "", "", false},
		{":rule-1", "", "", false},
		{"sg-1:rule-1:x", "", "", false},
	} {
		group, rule, ok := splitRuleImportID(tc.id)
		if ok != tc.ok || group != tc.group || rule != tc.rule {
			t.Errorf("%q: got (%q, %q, %v)", tc.id, group, rule, ok)
		}
	}
}

// Importing a default egress rule and reading it fills in the attributes its
// configuration declares (egress, 0.0.0.0/0, no protocol, no ports), so the
// plan that follows is empty.
func TestImportedDefaultEgressRuleReadsBackItsShape(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/network/security-groups" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"security_groups":[{"id":"sg-1","name":"web","egress_filtering":true,"rules":[
			{"id":"rule-4","security_group_id":"sg-1","direction":"egress","action":"accept","remote_ip_prefix":"0.0.0.0/0"},
			{"id":"rule-6","security_group_id":"sg-1","direction":"egress","action":"accept","remote_ip_prefix":"::/0"}]}]}`))
	}))
	defer srv.Close()
	client, err := nemau.NewClient(nemau.WithBaseURL(srv.URL), nemau.WithCredential(nemau.AccessKey{ID: "AKIA", Secret: "s3cr3t"}))
	if err != nil {
		t.Fatal(err)
	}
	r := &securityGroupRuleResource{client: client}
	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	objectType := schemaResp.Schema.Type().TerraformType(ctx).(tftypes.Object)
	empty := tfsdk.State{Schema: schemaResp.Schema, Raw: tftypes.NewValue(objectType, nil)}

	importResp := resource.ImportStateResponse{State: empty}
	r.ImportState(ctx, resource.ImportStateRequest{ID: "sg-1:rule-4"}, &importResp)
	if importResp.Diagnostics.HasError() {
		t.Fatalf("import: %v", importResp.Diagnostics)
	}
	readResp := resource.ReadResponse{State: importResp.State}
	r.Read(ctx, resource.ReadRequest{State: importResp.State}, &readResp)
	if readResp.Diagnostics.HasError() {
		t.Fatalf("read: %v", readResp.Diagnostics)
	}
	var rule securityGroupRuleModel
	readResp.Diagnostics.Append(readResp.State.Get(ctx, &rule)...)
	if rule.ID.ValueString() != "rule-4" || rule.SecurityGroupID.ValueString() != "sg-1" ||
		rule.Direction.ValueString() != "egress" || rule.RemoteIPPrefix.ValueString() != "0.0.0.0/0" ||
		!rule.Protocol.IsNull() || !rule.PortRangeMin.IsNull() {
		t.Fatalf("imported rule = %+v", rule)
	}

	bad := resource.ImportStateResponse{State: empty}
	r.ImportState(ctx, resource.ImportStateRequest{ID: "rule-4"}, &bad)
	if !bad.Diagnostics.HasError() {
		t.Fatal("an import id without the group must be refused")
	}
}
