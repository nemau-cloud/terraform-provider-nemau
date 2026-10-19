package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/float64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

// ---------------------------------------------------------------------------
// Keypair
// ---------------------------------------------------------------------------

var (
	_ resource.Resource              = (*keypairResource)(nil)
	_ resource.ResourceWithConfigure = (*keypairResource)(nil)
)

func NewKeypairResource() resource.Resource { return &keypairResource{} }

type keypairResource struct{ client *nemau.Client }

type keypairModel struct {
	Name        types.String `tfsdk:"name"`
	PublicKey   types.String `tfsdk:"public_key"`
	PrivateKey  types.String `tfsdk:"private_key"`
	Fingerprint types.String `tfsdk:"fingerprint"`
	ID          types.String `tfsdk:"id"`
}

func (r *keypairResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_keypair"
}

func (r *keypairResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An SSH keypair usable as an instance's `key_name`.\n\n" +
			"Without `public_key`, the platform generates an Ed25519 pair and returns the " +
			"private key once. Terraform stores it in state, and it cannot be retrieved anywhere " +
			"else. If your state is not encrypted, supply your own `public_key` instead.\n\n" +
			"Do not use a slash in the name: such a keypair can be created but not deleted.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Same as `name`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Keypair name. It also serves as the id. Changing it replaces the keypair.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"public_key": schema.StringAttribute{
				Optional: true, Computed: true,
				MarkdownDescription: "OpenSSH public key to register. When omitted, a pair is generated.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"private_key": schema.StringAttribute{
				Computed:  true,
				Sensitive: true,
				MarkdownDescription: "The generated private key, empty when `public_key` was " +
					"supplied. Not retrievable afterwards.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"fingerprint": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Fingerprint of the public key, as the platform reports it.",
			},
		},
	}
}

func (r *keypairResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromResourceProvider(req, resp)
}

func (r *keypairResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan keypairModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Unlike the other creates, this endpoint is synchronous: a generated private
	// key is only returned in this response, so there is no task to wait on.
	created, err := r.client.Compute.CreateKeypair(ctx, plan.Name.ValueString(), plan.PublicKey.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Creating the keypair was refused", err.Error())
		return
	}

	plan.ID = types.StringValue(plan.Name.ValueString())
	plan.PrivateKey = types.StringValue(created.PrivateKey)
	if created.PublicKey != "" {
		plan.PublicKey = types.StringValue(created.PublicKey)
	}
	r.fillFromList(ctx, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *keypairResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state keypairModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	keypairs, err := r.client.Compute.ListKeypairs(ctx)
	if handleReadError(err, "keypair", resp) {
		return
	}
	for _, keypair := range keypairs {
		if keypair.Name == state.Name.ValueString() {
			state.ID = types.StringValue(keypair.Name)
			state.Fingerprint = str(keypair.Fingerprint)
			if keypair.PublicKey != "" {
				state.PublicKey = types.StringValue(keypair.PublicKey)
			}
			resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
			return
		}
	}
	// The keypair is missing on the platform: remove it from state so the next
	// plan recreates it. The private key of a generated pair cannot be recovered.
	resp.State.RemoveResource(ctx)
}

func (r *keypairResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan keypairModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *keypairResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state keypairModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleteAndWait(ctx, r.client, &resp.Diagnostics, "keypair", func() (*nemau.TaskRef, error) {
		return r.client.Compute.DeleteKeypair(ctx, state.Name.ValueString())
	})
}

func (r *keypairResource) fillFromList(ctx context.Context, model *keypairModel) {
	keypairs, err := r.client.Compute.ListKeypairs(ctx)
	if err != nil {
		model.Fingerprint = types.StringNull()
		return
	}
	for _, keypair := range keypairs {
		if keypair.Name == model.Name.ValueString() {
			model.Fingerprint = str(keypair.Fingerprint)
			if model.PublicKey.IsUnknown() || model.PublicKey.IsNull() {
				model.PublicKey = str(keypair.PublicKey)
			}
			return
		}
	}
	model.Fingerprint = types.StringNull()
}

// ---------------------------------------------------------------------------
// Snapshot
// ---------------------------------------------------------------------------

var (
	_ resource.Resource                = (*snapshotResource)(nil)
	_ resource.ResourceWithConfigure   = (*snapshotResource)(nil)
	_ resource.ResourceWithImportState = (*snapshotResource)(nil)
)

func NewSnapshotResource() resource.Resource { return &snapshotResource{} }

type snapshotResource struct{ client *nemau.Client }

type snapshotModel struct {
	ID           types.String  `tfsdk:"id"`
	Name         types.String  `tfsdk:"name"`
	InstanceID   types.String  `tfsdk:"instance_id"`
	Status       types.String  `tfsdk:"status"`
	Storage      types.String  `tfsdk:"storage"`
	SizeBytes    types.Int64   `tfsdk:"size_bytes"`
	SourceDiskGB types.Int64   `tfsdk:"source_disk_gb"`
	MonthlyCost  types.Float64 `tfsdk:"monthly_cost"`
}

func (r *snapshotResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_snapshot"
}

func (r *snapshotResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A crash-consistent copy of an instance's system disk, stored on the object store " +
			"and billed on the bytes actually stored. Attached volumes are never included. Immutable: any change " +
			"recreates it.\n\n" +
			"Creation waits until the disk has reached the object store, which can take hours for a large disk; " +
			"the provider sets no timeout of its own. Deleting the instance deletes its snapshots.\n\n" +
			"Terraform does not restore snapshots. Use `nemau compute snapshot restore <id>` " +
			"or one of the SDKs.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Snapshot id, the value `nemau compute snapshot restore` takes.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Snapshot name.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"instance_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Id of the instance whose system disk is captured.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"status": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`creating`, `active` or `restoring`.",
			},
			"storage": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "`object_store`, or `hypervisor` for an older snapshot kept on the hypervisor. " +
					"Those can be listed and deleted, but are not billed and cannot be restored.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"size_bytes": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Bytes the object store holds for this snapshot (the compressed archive). This is the billed size.",
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"source_disk_gb": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Capacity of the system disk that was captured, in GB.",
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"monthly_cost": schema.Float64Attribute{
				Computed:            true,
				MarkdownDescription: "Price excluding VAT of keeping `size_bytes` stored for a 730-hour month, in euros.",
				PlanModifiers:       []planmodifier.Float64{float64planmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *snapshotResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromResourceProvider(req, resp)
}

// Create waits for the whole upload. It uses Terraform's context, which has no
// deadline: a snapshot of a large disk can take hours, and a timeout here would
// abandon a task the platform is still running.
func (r *snapshotResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan snapshotModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := createAndWait(ctx, r.client, &resp.Diagnostics, "snapshot", func() (*nemau.TaskRef, error) {
		return r.client.Compute.CreateSnapshot(ctx, plan.InstanceID.ValueString(), plan.Name.ValueString())
	}, "snapshot_id")
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(id)
	// Size and price are only known once the upload has finished, so they are
	// read back rather than guessed.
	snapshots, err := r.client.Compute.ListSnapshots(ctx, plan.InstanceID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Reading the new snapshot failed", err.Error())
		return
	}
	if snapshot, ok := findSnapshot(snapshots, id); ok {
		applySnapshot(&plan, snapshot)
	} else {
		applySnapshot(&plan, nemau.Snapshot{Name: plan.Name.ValueString(), InstanceID: plan.InstanceID.ValueString()})
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *snapshotResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state snapshotModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	snapshots, err := r.client.Compute.ListSnapshots(ctx, state.InstanceID.ValueString())
	if handleReadError(err, "snapshot", resp) {
		return
	}
	if snapshot, ok := findSnapshot(snapshots, state.ID.ValueString()); ok {
		applySnapshot(&state, snapshot)
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
		return
	}
	resp.State.RemoveResource(ctx)
}

func findSnapshot(snapshots []nemau.Snapshot, id string) (nemau.Snapshot, bool) {
	for _, snapshot := range snapshots {
		if snapshot.ID == id {
			return snapshot, true
		}
	}
	return nemau.Snapshot{}, false
}

// applySnapshot copies what the API reports onto the model. Size, disk and
// price are null rather than zero when the API does not report them: a
// snapshot still uploading, or one kept on the hypervisor, has no stored size.
func applySnapshot(model *snapshotModel, snapshot nemau.Snapshot) {
	model.Name = types.StringValue(snapshot.Name)
	model.InstanceID = types.StringValue(snapshot.InstanceID)
	model.Status = str(snapshot.Status)
	model.Storage = str(snapshot.Storage)
	model.SizeBytes = types.Int64PointerValue(snapshot.SizeBytes)
	model.SourceDiskGB = types.Int64Null()
	if snapshot.SourceDiskGB != nil {
		model.SourceDiskGB = types.Int64Value(int64(*snapshot.SourceDiskGB))
	}
	model.MonthlyCost = types.Float64PointerValue(snapshot.MonthlyCost)
}

func (r *snapshotResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan snapshotModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *snapshotResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state snapshotModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleteAndWait(ctx, r.client, &resp.Diagnostics, "snapshot", func() (*nemau.TaskRef, error) {
		return r.client.Compute.DeleteSnapshot(ctx, state.ID.ValueString())
	})
}

func (r *snapshotResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importByID(ctx, req, resp)
}
