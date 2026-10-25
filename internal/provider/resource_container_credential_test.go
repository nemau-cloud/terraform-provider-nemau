package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

const credFakeSecret = "EXAMPLE-SECRET-NOT-REAL-0123456789"

// credFakeBucket is the store's name for "photos": what S3 tools must address.
const credFakeBucket = "nemau-dev-00000000000000000000000000000000"

type credentialAPI struct {
	t        *testing.T
	requests []string
	bodies   []string
	// createStatus/createBody override the POST answer; list is the GET body.
	createStatus int
	createBody   string
	listStatus   int
	listBody     string
	deleteStatus int
	deleteBody   string
	// containersBody answers GET /storage/containers (the container listing).
	containersBody string
}

func (a *credentialAPI) client() *nemau.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		a.requests = append(a.requests, r.Method+" "+r.URL.Path)
		a.bodies = append(a.bodies, string(raw))
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			status, body := a.createStatus, a.createBody
			if status == 0 {
				status = http.StatusCreated
			}
			if body == "" {
				body = issuedCredentialJSON("")
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		case http.MethodGet:
			if r.URL.Path == "/api/v1/storage/containers" {
				body := a.containersBody
				if body == "" {
					body = `{"containers":[]}`
				}
				_, _ = w.Write([]byte(body))
				return
			}
			status, body := a.listStatus, a.listBody
			if status == 0 {
				status = http.StatusOK
			}
			if body == "" {
				body = `{"credentials":[]}`
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		case http.MethodDelete:
			status := a.deleteStatus
			if status == 0 {
				status = http.StatusNoContent
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(a.deleteBody))
		}
	}))
	a.t.Cleanup(srv.Close)
	c, err := nemau.NewClient(
		nemau.WithBaseURL(srv.URL),
		nemau.WithCredential(nemau.AccessKey{ID: "fake", Secret: "fake"}),
		nemau.WithRetry(nemau.RetryPolicy{MaxAttempts: 1}),
	)
	if err != nil {
		a.t.Fatalf("NewClient: %v", err)
	}
	return c
}

func issuedCredentialJSON(expires string) string {
	exp := "null"
	if expires != "" {
		exp = `"` + expires + `"`
	}
	return `{"access_key_id":"AKEXAMPLE0000000001","secret_access_key":"` + credFakeSecret + `","access":"read_only",
		"container":"photos","s3_bucket":"` + credFakeBucket + `","region":"fr-occitanie-1","endpoint":"","created_at":"2026-10-04T16:06:25Z","expires_at":` + exp + `}`
}

func credentialResource(t *testing.T, c *nemau.Client) (resource.Resource, schema.Schema) {
	t.Helper()
	r := NewContainerCredentialResource()
	r.(resource.ResourceWithConfigure).Configure(context.Background(),
		resource.ConfigureRequest{ProviderData: c}, &resource.ConfigureResponse{})
	var sr resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &sr)
	return r, sr.Schema
}

// credentialValue builds an object value for the schema: attributes not named are null.
func credentialValue(t *testing.T, s schema.Schema, set map[string]string) tftypes.Value {
	t.Helper()
	typ := s.Type().TerraformType(context.Background()).(tftypes.Object)
	values := map[string]tftypes.Value{}
	for name, attrType := range typ.AttributeTypes {
		if v, ok := set[name]; ok {
			values[name] = tftypes.NewValue(attrType, v)
		} else {
			values[name] = tftypes.NewValue(attrType, nil)
		}
	}
	return tftypes.NewValue(typ, values)
}

func credentialPlan(t *testing.T, s schema.Schema, set map[string]string) tfsdk.Plan {
	return tfsdk.Plan{Schema: s, Raw: credentialValue(t, s, set)}
}

func emptyState(s schema.Schema) tfsdk.State {
	return tfsdk.State{Schema: s, Raw: tftypes.NewValue(s.Type().TerraformType(context.Background()), nil)}
}

func readModel(t *testing.T, state tfsdk.State) containerCredentialModel {
	t.Helper()
	var m containerCredentialModel
	if diags := state.Get(context.Background(), &m); diags.HasError() {
		t.Fatalf("state: %v", diags)
	}
	return m
}

func TestContainerCredentialCreateKeepsTheSecretAndTheSpellingOfExpiry(t *testing.T) {
	api := &credentialAPI{t: t, createBody: issuedCredentialJSON("2030-01-01T00:00:00Z")}
	r, s := credentialResource(t, api.client())

	resp := resource.CreateResponse{State: emptyState(s)}
	r.Create(context.Background(), resource.CreateRequest{Plan: credentialPlan(t, s, map[string]string{
		"container": "photos", "access": "read_only", "expires_at": "2030-01-01T01:00:00+01:00",
	})}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}

	m := readModel(t, resp.State)
	if m.SecretAccessKey.ValueString() != credFakeSecret || m.AccessKeyID.ValueString() != "AKEXAMPLE0000000001" {
		t.Errorf("secret/key not stored: %+v", m)
	}
	if m.ID.ValueString() != "photos/AKEXAMPLE0000000001" || m.Region.ValueString() != "fr-occitanie-1" {
		t.Errorf("id/region: %+v", m)
	}
	// The bucket S3 tools address is the store's name, not the container's.
	if m.S3Bucket.ValueString() != credFakeBucket || m.Container.ValueString() != "photos" {
		t.Errorf("s3_bucket = %v, container = %v", m.S3Bucket, m.Container)
	}
	// An empty endpoint is a value ("none to show"), not null: null would be unknown after apply.
	if m.Endpoint.IsNull() || m.Endpoint.ValueString() != "" {
		t.Errorf("endpoint = %v, want the empty string", m.Endpoint)
	}
	// The value keeps the spelling from the configuration, not the platform's normalised one.
	if m.ExpiresAt.ValueString() != "2030-01-01T01:00:00+01:00" {
		t.Errorf("expires_at rewritten to %q", m.ExpiresAt.ValueString())
	}
	if len(api.requests) != 1 || api.requests[0] != "POST /api/v1/storage/containers/photos/credentials" {
		t.Errorf("requests = %v", api.requests)
	}
	if !strings.Contains(api.bodies[0], `"access":"read_only"`) || !strings.Contains(api.bodies[0], `"expires_at":"2030-01-01T00:00:00Z"`) {
		t.Errorf("body = %s", api.bodies[0])
	}
}

func TestContainerCredentialCreateWithoutExpirySendsNone(t *testing.T) {
	api := &credentialAPI{t: t}
	r, s := credentialResource(t, api.client())
	resp := resource.CreateResponse{State: emptyState(s)}
	r.Create(context.Background(), resource.CreateRequest{Plan: credentialPlan(t, s, map[string]string{
		"container": "photos", "access": "read_only",
	})}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics)
	}
	if strings.Contains(api.bodies[0], "expires_at") {
		t.Errorf("body = %s, want no expires_at", api.bodies[0])
	}
	if m := readModel(t, resp.State); !m.ExpiresAt.IsNull() {
		t.Errorf("expires_at = %v, want null (plan stability)", m.ExpiresAt)
	}
}

func TestContainerCredentialCreateRefusalsExplainTheWayOut(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   []string
	}{
		{"limit", 429, `{"error":"this container already has the maximum number of access keys","code":"credential_limit"}`,
			[]string{"reached its access key limit", "maximum number of access keys",
				"nemau storage credentials list photos", "nemau storage credentials revoke photos"}},
		{"limit with its cap", 429, `{"error":"limit reached","code":"credential_limit","limit":10,"scope":"bucket"}`,
			[]string{"reached its access key limit", "Limit: 10 keys per bucket."}},
		{"read only org", 403, `{"error":"the organization is read-only","code":"organization_read_only"}`,
			[]string{"organization is read-only", `access = "read_only"`}},
		{"other", 429, `{"error":"rate limit exceeded"}`, []string{"was refused", "rate limit exceeded"}},
	}
	for _, tc := range cases {
		api := &credentialAPI{t: t, createStatus: tc.status, createBody: tc.body}
		r, s := credentialResource(t, api.client())
		resp := resource.CreateResponse{State: emptyState(s)}
		r.Create(context.Background(), resource.CreateRequest{Plan: credentialPlan(t, s, map[string]string{
			"container": "photos", "access": "read_write",
		})}, &resp)
		if !resp.Diagnostics.HasError() {
			t.Fatalf("%s: no error", tc.name)
		}
		text := resp.Diagnostics[0].Summary() + "\n" + resp.Diagnostics[0].Detail()
		for _, want := range tc.want {
			if !strings.Contains(text, want) {
				t.Errorf("%s: diagnostic missing %q:\n%s", tc.name, want, text)
			}
		}
		if !resp.State.Raw.IsNull() {
			t.Errorf("%s: a refused creation left a state behind", tc.name)
		}
	}
}

func stateWithKey(t *testing.T, s schema.Schema, extra map[string]string) tfsdk.State {
	set := map[string]string{
		"id": "photos/AKEXAMPLE0000000001", "container": "photos", "access": "read_only",
		"access_key_id": "AKEXAMPLE0000000001", "secret_access_key": credFakeSecret,
		"region": "fr-occitanie-1", "endpoint": "", "created_at": "2026-10-04T16:06:25Z",
		"s3_bucket": credFakeBucket,
	}
	for k, v := range extra {
		set[k] = v
	}
	return tfsdk.State{Schema: s, Raw: credentialValue(t, s, set)}
}

const listedKey = `{"credentials":[{"access_key_id":"AKEXAMPLE0000000001","access":"read_only","container":"photos","created_at":"2026-10-04T16:06:25Z","expires_at":null}]}`

func TestContainerCredentialReadNeverTouchesTheSecretOrExpiry(t *testing.T) {
	api := &credentialAPI{t: t, listBody: listedKey}
	r, s := credentialResource(t, api.client())

	resp := resource.ReadResponse{State: stateWithKey(t, s, nil)}
	r.Read(context.Background(), resource.ReadRequest{State: stateWithKey(t, s, nil)}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics)
	}
	if resp.State.Raw.IsNull() {
		t.Fatalf("Read removed the key (requests=%v)", api.requests)
	}
	m := readModel(t, resp.State)
	if m.SecretAccessKey.ValueString() != credFakeSecret {
		t.Error("Read overwrote the secret")
	}
	if !m.ExpiresAt.IsNull() {
		t.Errorf("Read set expires_at to %v on a key created without one", m.ExpiresAt)
	}
	if len(api.requests) != 1 || api.requests[0] != "GET /api/v1/storage/containers/photos/credentials" {
		t.Errorf("requests = %v", api.requests)
	}
}

// A key created before the platform reported the bucket gets it on refresh, from the
// container listing. It is never left null and never set to the container name.
func TestContainerCredentialReadFillsTheBucketOfAnOlderState(t *testing.T) {
	api := &credentialAPI{t: t, listBody: listedKey,
		containersBody: `{"containers":[{"id":"c-1","name":"photos","bytes":0,"count":0,"s3_bucket":"` + credFakeBucket + `"}]}`}
	r, s := credentialResource(t, api.client())
	older := stateWithKey(t, s, nil)
	if diags := older.SetAttribute(context.Background(), tfpathRoot("s3_bucket"), types.StringNull()); diags.HasError() {
		t.Fatal(diags)
	}
	resp := resource.ReadResponse{State: older}
	r.Read(context.Background(), resource.ReadRequest{State: older}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics)
	}
	if m := readModel(t, resp.State); m.S3Bucket.ValueString() != credFakeBucket {
		t.Errorf("s3_bucket = %v, want %s", m.S3Bucket, credFakeBucket)
	}
}

func TestContainerCredentialReadDropsAKeyThatIsGone(t *testing.T) {
	for name, api := range map[string]*credentialAPI{
		"key absent from the listing": {t: t, listBody: `{"credentials":[{"access_key_id":"AKOTHER","access":"read_only","container":"photos","created_at":"2026-10-04T16:06:25Z"}]}`},
		"container deleted":           {t: t, listStatus: 404, listBody: `{"error":"container not found"}`},
	} {
		r, s := credentialResource(t, api.client())
		resp := resource.ReadResponse{State: stateWithKey(t, s, nil)}
		r.Read(context.Background(), resource.ReadRequest{State: stateWithKey(t, s, nil)}, &resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("%s: %v", name, resp.Diagnostics)
		}
		if !resp.State.Raw.IsNull() {
			t.Errorf("%s: the key is gone and must be removed from the state", name)
		}
	}
}

func TestContainerCredentialReadReportsAServerErrorInsteadOfDroppingTheKey(t *testing.T) {
	api := &credentialAPI{t: t, listStatus: 500, listBody: `{"error":"boom"}`}
	r, s := credentialResource(t, api.client())
	resp := resource.ReadResponse{State: stateWithKey(t, s, nil)}
	r.Read(context.Background(), resource.ReadRequest{State: stateWithKey(t, s, nil)}, &resp)
	if !resp.Diagnostics.HasError() || resp.State.Raw.IsNull() {
		t.Errorf("a 500 must be an error, not a silent removal (diags=%v)", resp.Diagnostics)
	}
}

func TestContainerCredentialDelete(t *testing.T) {
	for name, tc := range map[string]struct {
		status  int
		wantErr bool
	}{"revoked": {204, false}, "already gone is success": {404, false}, "server error": {500, true}} {
		api := &credentialAPI{t: t, deleteStatus: tc.status, deleteBody: `{"error":"x"}`}
		r, s := credentialResource(t, api.client())
		resp := resource.DeleteResponse{}
		r.Delete(context.Background(), resource.DeleteRequest{State: stateWithKey(t, s, nil)}, &resp)
		if resp.Diagnostics.HasError() != tc.wantErr {
			t.Errorf("%s: diagnostics = %v", name, resp.Diagnostics)
		}
		if got := api.requests[0]; got != "DELETE /api/v1/storage/containers/photos/credentials/AKEXAMPLE0000000001" {
			t.Errorf("%s: request = %s", name, got)
		}
	}
}

func TestContainerCredentialValidateConfig(t *testing.T) {
	r, s := credentialResource(t, nil)
	validate := func(set map[string]string) int {
		resp := resource.ValidateConfigResponse{}
		r.(resource.ResourceWithValidateConfig).ValidateConfig(context.Background(),
			resource.ValidateConfigRequest{Config: tfsdk.Config{Schema: s, Raw: credentialValue(t, s, set)}}, &resp)
		return resp.Diagnostics.ErrorsCount()
	}
	if n := validate(map[string]string{"container": "photos", "access": "read_write", "expires_at": "2030-01-01T00:00:00Z"}); n != 0 {
		t.Errorf("a valid config has %d error(s)", n)
	}
	if n := validate(map[string]string{"container": "photos", "access": "admin"}); n != 1 {
		t.Errorf("access=admin: %d error(s), want 1", n)
	}
	if n := validate(map[string]string{"container": "photos", "access": "read_only", "expires_at": "tomorrow"}); n != 1 {
		t.Errorf("expires_at=tomorrow: %d error(s), want 1", n)
	}
}

// Every writable argument forces a replacement (a key is never edited), the secret is
// Sensitive, and an unset expiry stays null so it never produces a diff.
func TestContainerCredentialSchemaContract(t *testing.T) {
	_, s := credentialResource(t, nil)
	for _, name := range []string{"container", "access", "expires_at"} {
		attr, ok := s.Attributes[name].(schema.StringAttribute)
		if !ok || len(attr.PlanModifiers) == 0 {
			t.Errorf("%s must force a replacement", name)
		}
	}
	if secret := s.Attributes["secret_access_key"]; !secret.IsSensitive() || !secret.IsComputed() || secret.IsRequired() || secret.IsOptional() {
		t.Error("secret_access_key must be Computed and Sensitive")
	}
	// The bucket name is useless without the key, so it is not Sensitive and shows in outputs.
	if bucket := s.Attributes["s3_bucket"]; bucket == nil || !bucket.IsComputed() || bucket.IsOptional() || bucket.IsSensitive() {
		t.Error("s3_bucket must be Computed only")
	}
	if expires := s.Attributes["expires_at"]; !expires.IsOptional() || expires.IsComputed() {
		t.Error("expires_at must be Optional and not Computed, or an unset expiry would show as a diff")
	}
	if _, importable := NewContainerCredentialResource().(resource.ResourceWithImportState); importable {
		t.Error("a key must not be importable: its secret is unrecoverable")
	}
}

func TestCredentialLimitSentenceOnlyStatesWhatThePlatformSaid(t *testing.T) {
	apiErr := func(limit int, scope string) error {
		return &nemau.APIError{StatusCode: 429, Code: nemau.ErrorCodeCredentialLimit, Limit: limit, Scope: scope}
	}
	for _, tc := range []struct {
		err  error
		want string
	}{
		{apiErr(10, "bucket"), "\n\nLimit: 10 keys per bucket."},
		{apiErr(50, "project"), "\n\nLimit: 50 keys per project."},
		{apiErr(7, ""), "\n\nLimit: 7 keys."},
		{apiErr(0, ""), ""},
		{apiErr(0, "bucket"), ""},
	} {
		if got := credentialLimitSentence(tc.err); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.err, got, tc.want)
		}
	}
}
