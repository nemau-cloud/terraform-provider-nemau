package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

func containerResourceFor(t *testing.T, c *nemau.Client) (resource.Resource, schema.Schema) {
	t.Helper()
	r := NewContainerResource()
	r.(resource.ResourceWithConfigure).Configure(context.Background(),
		resource.ConfigureRequest{ProviderData: c}, &resource.ConfigureResponse{})
	var sr resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &sr)
	return r, sr.Schema
}

// containerState is a state with name "photos" and every other attribute null, or set to
// the given tftypes values.
func containerState(t *testing.T, s schema.Schema, set map[string]tftypes.Value) tftypes.Value {
	t.Helper()
	typ := s.Type().TerraformType(context.Background()).(tftypes.Object)
	values := map[string]tftypes.Value{}
	for name, attrType := range typ.AttributeTypes {
		values[name] = tftypes.NewValue(attrType, nil)
	}
	values["id"] = tftypes.NewValue(tftypes.String, "photos")
	values["name"] = tftypes.NewValue(tftypes.String, "photos")
	for k, v := range set {
		values[k] = v
	}
	return tftypes.NewValue(typ, values)
}

const photosListing = `{"containers":[{"id":"c-1","name":"photos","bytes":12,"count":1,"s3_bucket":"` + credFakeBucket + `"}]}`

// Read publishes the bucket S3 tools need, from the container listing.
func TestContainerReadSetsTheS3Bucket(t *testing.T) {
	api := &credentialAPI{t: t, containersBody: photosListing}
	r, s := containerResourceFor(t, api.client())
	state := tfsdk.State{Schema: s, Raw: containerState(t, s, nil)}
	resp := resource.ReadResponse{State: state}
	r.Read(context.Background(), resource.ReadRequest{State: state}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics)
	}
	var m containerModel
	if diags := resp.State.Get(context.Background(), &m); diags.HasError() {
		t.Fatal(diags)
	}
	if m.S3Bucket.ValueString() != credFakeBucket || m.Name.ValueString() != "photos" {
		t.Errorf("s3_bucket = %v, name = %v", m.S3Bucket, m.Name)
	}
}

// A state written before s3_bucket existed plans it unknown; Update must settle it.
func TestContainerUpdateNeverLeavesTheS3BucketUnknown(t *testing.T) {
	api := &credentialAPI{t: t, containersBody: photosListing}
	r, s := containerResourceFor(t, api.client())
	plan := tfsdk.Plan{Schema: s, Raw: containerState(t, s, map[string]tftypes.Value{
		"s3_bucket": tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
	})}
	prior := tfsdk.State{Schema: s, Raw: containerState(t, s, nil)}
	resp := resource.UpdateResponse{State: prior}
	r.Update(context.Background(), resource.UpdateRequest{Plan: plan, State: prior}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics)
	}
	var m containerModel
	if diags := resp.State.Get(context.Background(), &m); diags.HasError() {
		t.Fatal(diags)
	}
	if m.S3Bucket.IsUnknown() || m.S3Bucket.ValueString() != credFakeBucket {
		t.Errorf("s3_bucket = %v", m.S3Bucket)
	}
}

func TestContainerS3BucketIsComputedOnly(t *testing.T) {
	_, s := containerResourceFor(t, nil)
	if bucket := s.Attributes["s3_bucket"]; bucket == nil || !bucket.IsComputed() || bucket.IsOptional() || bucket.IsRequired() {
		t.Error("s3_bucket must be Computed only")
	}
}
