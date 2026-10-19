package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

// ---------------------------------------------------------------------------
// Bucket-scoped S3 access key
// ---------------------------------------------------------------------------

var (
	_ resource.Resource                   = (*containerCredentialResource)(nil)
	_ resource.ResourceWithConfigure      = (*containerCredentialResource)(nil)
	_ resource.ResourceWithValidateConfig = (*containerCredentialResource)(nil)
)

func NewContainerCredentialResource() resource.Resource { return &containerCredentialResource{} }

type containerCredentialResource struct{ client *nemau.Client }

type containerCredentialModel struct {
	ID              types.String `tfsdk:"id"`
	Container       types.String `tfsdk:"container"`
	Access          types.String `tfsdk:"access"`
	ExpiresAt       types.String `tfsdk:"expires_at"`
	AccessKeyID     types.String `tfsdk:"access_key_id"`
	SecretAccessKey types.String `tfsdk:"secret_access_key"`
	S3Bucket        types.String `tfsdk:"s3_bucket"`
	Region          types.String `tfsdk:"region"`
	Endpoint        types.String `tfsdk:"endpoint"`
	CreatedAt       types.String `tfsdk:"created_at"`
}

func (r *containerCredentialResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_container_credential"
}

func (r *containerCredentialResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An S3 access key scoped to one object container, read-only or read-write.\n\n" +
			"The secret is stored in the Terraform state. The platform returns it once, when the " +
			"key is created, and cannot recover it, so the state holds the only copy. " +
			"Use an encrypted remote backend and restrict who can read the state. The attribute is " +
			"marked sensitive, which hides it from plan and apply output but not from the state file.\n\n" +
			"Creating and revoking keys requires permission to manage storage credentials " +
			"(`storage:ManageCredentials`). A read-only organization can only create `read_only` keys. " +
			"Each container holds a limited number of keys; when the limit is reached, the apply fails " +
			"with the platform's message, and `nemau storage credentials list` and " +
			"`nemau storage credentials revoke` free a slot.\n\n" +
			"Changing any argument creates a new key; a key cannot be edited in place. A key revoked " +
			"outside Terraform leaves the state at the next refresh, and the next plan creates a new " +
			"one. An expired key leaves the state too; change or remove `expires_at` to create a new " +
			"one. Destroying the resource revokes the key, and a key that is already gone " +
			"counts as destroyed. Deleting the container revokes its keys.\n\n" +
			"Keep `container` pointing at the `nemau_container` resource: that reference makes " +
			"Terraform destroy the keys before the container.\n\n" +
			"S3 tools address `s3_bucket` rather than `container`. The object store knows each " +
			"container under an opaque bucket name, and the key's policy only covers that bucket.\n\n" +
			"This resource cannot be imported, because the secret of an existing key cannot be recovered. " +
			"Create a new key instead, and revoke the old one with " +
			"`nemau storage credentials revoke <container> <access-key-id>`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Synthetic id `<container>/<access_key_id>`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"container": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Name of the object container the key belongs to: `nemau_container.<name>.name`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"access": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "What the key may do: `read_only` (list and read objects) or " +
					"`read_write` (also write and delete objects). A key never changes container settings.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"expires_at": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "RFC 3339 time at which the platform revokes the key, for example " +
					"`2026-12-31T23:59:00Z`. It must be in the future and within the platform's maximum " +
					"lifetime. Omit it for a key that never expires. Terraform does not recreate the key " +
					"after it expires: remove or change the attribute to get a new one.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"access_key_id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The public half of the key.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"secret_access_key": schema.StringAttribute{
				Computed:            true,
				Sensitive:           true,
				MarkdownDescription: "The secret half of the key. Known after creation only; kept in the state.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"s3_bucket": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Bucket name to address in S3 tooling (`s3://<s3_bucket>`, " +
					"`remote:<s3_bucket>`). It differs from `container`: the object store knows each " +
					"container under an opaque name, and the key's policy covers that name.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"region": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Region to configure in S3 tooling (the platform region).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"endpoint": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "S3 endpoint to configure in S3 tooling. Empty when the platform has " +
					"no reachable endpoint to show (a demonstration environment).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Creation time, RFC 3339.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *containerCredentialResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromResourceProvider(req, resp)
}

// ValidateConfig checks what can be checked without a round trip. The platform
// decides everything else (the future-and-within-limit rule on expires_at).
func (r *containerCredentialResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config containerCredentialModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !config.Access.IsUnknown() && !config.Access.IsNull() {
		if a := config.Access.ValueString(); a != string(nemau.CredentialReadOnly) && a != string(nemau.CredentialReadWrite) {
			resp.Diagnostics.AddAttributeError(tfpathRoot("access"), "Unknown access level",
				fmt.Sprintf("access must be %q or %q, got %q.", nemau.CredentialReadOnly, nemau.CredentialReadWrite, a))
		}
	}
	if !config.ExpiresAt.IsUnknown() && !config.ExpiresAt.IsNull() {
		if _, err := time.Parse(time.RFC3339, config.ExpiresAt.ValueString()); err != nil {
			resp.Diagnostics.AddAttributeError(tfpathRoot("expires_at"), "Invalid expiry",
				"expires_at must be an RFC 3339 time such as 2026-12-31T23:59:00Z.")
		}
	}
}

func (r *containerCredentialResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan containerCredentialModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	request := nemau.CreateCredentialRequest{Access: nemau.CredentialAccess(plan.Access.ValueString())}
	if !plan.ExpiresAt.IsNull() && !plan.ExpiresAt.IsUnknown() {
		when, err := time.Parse(time.RFC3339, plan.ExpiresAt.ValueString())
		if err != nil {
			resp.Diagnostics.AddAttributeError(tfpathRoot("expires_at"), "Invalid expiry", err.Error())
			return
		}
		request.ExpiresAt = when
	}

	container := plan.Container.ValueString()
	issued, err := r.client.Storage.CreateCredential(ctx, container, request)
	if err != nil {
		addCredentialCreateError(&resp.Diagnostics, err, container)
		return
	}

	// The secret is only returned in this response and the state is the only
	// place it is kept, so write it before anything that could fail.
	plan.ID = types.StringValue(container + "/" + issued.AccessKeyID)
	plan.AccessKeyID = types.StringValue(issued.AccessKeyID)
	plan.SecretAccessKey = types.StringValue(issued.SecretAccessKey)
	plan.S3Bucket = types.StringValue(issued.S3Bucket)
	plan.Region = types.StringValue(issued.Region)
	// An empty endpoint means there is no endpoint to show. Store the empty
	// string, because null would read back as a perpetual unknown.
	plan.Endpoint = types.StringValue(issued.Endpoint)
	plan.CreatedAt = types.StringValue(issued.CreatedAt.Format(time.RFC3339))
	// expires_at keeps the practitioner's own spelling: the platform echoes a
	// normalised form, and writing that back would show as a change.
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read confirms the key still exists. It never rewrites the secret (the
// platform cannot return it) nor the other attributes the key was created with.
func (r *containerCredentialResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state containerCredentialModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	keys, err := r.client.Storage.ListCredentials(ctx, state.Container.ValueString())
	if handleReadError(err, "container credential", resp) {
		return
	}
	for _, key := range keys {
		if key.AccessKeyID != state.AccessKeyID.ValueString() {
			continue
		}
		// access cannot change on a live key; if it ever differs the plan shows a
		// replacement instead of hiding the drift.
		state.Access = types.StringValue(string(key.Access))
		if state.S3Bucket.IsNull() || state.S3Bucket.IsUnknown() {
			state.S3Bucket = r.containerBucket(ctx, state.Container.ValueString())
		}
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
		return
	}
	// Revoked or expired outside Terraform: drop it so the next plan creates a new one.
	resp.State.RemoveResource(ctx)
}

// containerBucket fills s3_bucket for a key created before the platform
// reported it: the container's own listing carries the same name. A failed
// lookup leaves it null rather than failing a refresh that is about the key.
func (r *containerCredentialResource) containerBucket(ctx context.Context, container string) types.String {
	found, err := r.client.Storage.GetContainer(ctx, container)
	if err != nil || found == nil {
		return types.StringNull()
	}
	return types.StringValue(found.S3Bucket)
}

// Update is only ever reached with an unchanged plan: every argument forces a
// replacement.
func (r *containerCredentialResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan containerCredentialModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// A computed value is never left unknown after apply.
	if plan.S3Bucket.IsUnknown() {
		plan.S3Bucket = r.containerBucket(ctx, plan.Container.ValueString())
		if plan.S3Bucket.IsNull() {
			plan.S3Bucket = types.StringValue("")
		}
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *containerCredentialResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state containerCredentialModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	err := r.client.Storage.RevokeCredential(ctx, state.Container.ValueString(), state.AccessKeyID.ValueString())
	// Already gone (revoked, expired, or the container was deleted): the goal of a
	// destroy is reached, and failing here would strand an entry in the state.
	if err != nil && !nemau.IsNotFound(err) {
		resp.Diagnostics.AddError("Revoking the access key failed", err.Error())
	}
}

// credentialLimitSentence names the cap when the platform reported it, e.g.
// "\n\nLimit: 10 keys per bucket." Nothing when it did not.
func credentialLimitSentence(err error) string {
	limit, scope, _ := nemau.CredentialLimit(err)
	switch {
	case limit <= 0:
		return ""
	case scope == "":
		return fmt.Sprintf("\n\nLimit: %d keys.", limit)
	}
	return fmt.Sprintf("\n\nLimit: %d keys per %s.", limit, scope)
}

// addCredentialCreateError turns a refused creation into a diagnostic that says
// what to do. The platform's own message is always kept.
func addCredentialCreateError(diags interface {
	AddError(summary, detail string)
}, err error, container string) {
	switch {
	case nemau.IsCredentialLimit(err):
		diags.AddError("The container has reached its access key limit",
			err.Error()+credentialLimitSentence(err)+"\n\nRevoke a key you no longer use, then apply again. "+
				"To revoke a key, remove its `nemau_container_credential` from the configuration, "+
				"or run `nemau storage credentials list "+container+
				"` and `nemau storage credentials revoke "+container+" <access-key-id>`.")
	case nemau.IsOrganizationReadOnly(err):
		diags.AddError("The organization is read-only",
			err.Error()+"\n\nWhile the organization is read-only, only keys with `access = \"read_only\"` can be created.")
	default:
		diags.AddError("Creating the access key was refused", err.Error())
	}
}
