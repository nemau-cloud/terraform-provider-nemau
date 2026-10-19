package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

// ---------------------------------------------------------------------------
// Block volume
// ---------------------------------------------------------------------------

var (
	_ resource.Resource                = (*volumeResource)(nil)
	_ resource.ResourceWithConfigure   = (*volumeResource)(nil)
	_ resource.ResourceWithImportState = (*volumeResource)(nil)
	_ resource.ResourceWithModifyPlan  = (*volumeResource)(nil)
)

func NewVolumeResource() resource.Resource { return &volumeResource{} }

type volumeResource struct{ client *nemau.Client }

type volumeModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	Size        types.Int64  `tfsdk:"size"`
	Status      types.String `tfsdk:"status"`
}

func (r *volumeResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_volume"
}

func (r *volumeResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A block volume, in GiB.\n\n" +
			"`size` changes in place, but a volume can only grow. The plan refuses a smaller " +
			"size, because the API cannot shrink a volume.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Volume id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Volume name. Changes in place.",
			},
			"description": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Free-form description. Changes in place.",
			},
			"size": schema.Int64Attribute{
				Required:            true,
				MarkdownDescription: "Size in GiB. Can be increased in place, never reduced.",
			},
			"status": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Status as reported by the platform.",
			},
		},
	}
}

func (r *volumeResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromResourceProvider(req, resp)
}

// ModifyPlan refuses a shrink at plan time. The API would answer an error at
// apply, halfway through a run that may have already changed other resources.
func (r *volumeResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return // create or destroy: nothing to compare
	}
	var plan, state volumeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || plan.Size.IsUnknown() {
		return
	}
	if plan.Size.ValueInt64() < state.Size.ValueInt64() {
		resp.Diagnostics.AddAttributeError(tfpathRoot("size"), "A volume cannot shrink",
			"A volume can only grow. To start with a smaller volume, back up its data, then "+
				"destroy the volume explicitly, for example with `terraform taint`.")
	}
}

func (r *volumeResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan volumeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := createAndWait(ctx, r.client, &resp.Diagnostics, "volume", func() (*nemau.TaskRef, error) {
		return r.client.Storage.CreateVolume(ctx, plan.Name.ValueString(),
			plan.Description.ValueString(), int(plan.Size.ValueInt64()))
	}, "volume_id")
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(id)
	if volume, err := r.client.Storage.GetVolume(ctx, id); err == nil {
		plan.Status = str(volume.Status)
	} else {
		plan.Status = types.StringValue("unknown")
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *volumeResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state volumeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	volume, err := r.client.Storage.GetVolume(ctx, state.ID.ValueString())
	if handleReadError(err, "volume", resp) {
		return
	}
	state.Name = types.StringValue(volume.Name)
	state.Description = str(volume.Description)
	state.Size = types.Int64Value(int64(volume.Size))
	state.Status = str(volume.Status)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *volumeResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state volumeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = state.ID
	if plan.Size.ValueInt64() != state.Size.ValueInt64() {
		runAndWait(ctx, r.client, &resp.Diagnostics, "Volume extension", func() (*nemau.TaskRef, error) {
			return r.client.Storage.ExtendVolume(ctx, state.ID.ValueString(), int(plan.Size.ValueInt64()))
		})
		if resp.Diagnostics.HasError() {
			return
		}
	}

	// The name and description are sent separately from the size: growing a
	// volume and renaming it are separate operations, and a failure in one must
	// not be reported as a failure in the other.
	update := nemau.Update{}
	if !plan.Name.Equal(state.Name) {
		update.Name = nemau.Set(plan.Name.ValueString())
	}
	if !plan.Description.Equal(state.Description) {
		update.Description = nemau.Set(plan.Description.ValueString())
	}
	if update != (nemau.Update{}) {
		runAndWait(ctx, r.client, &resp.Diagnostics, "volume update", func() (*nemau.TaskRef, error) {
			return r.client.Storage.UpdateVolume(ctx, state.ID.ValueString(), update)
		})
		if resp.Diagnostics.HasError() {
			return
		}
	}
	if volume, err := r.client.Storage.GetVolume(ctx, state.ID.ValueString()); err == nil {
		plan.Status = str(volume.Status)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *volumeResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state volumeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleteAndWait(ctx, r.client, &resp.Diagnostics, "volume", func() (*nemau.TaskRef, error) {
		return r.client.Storage.DeleteVolume(ctx, state.ID.ValueString())
	})
}

func (r *volumeResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importByID(ctx, req, resp)
}

// ---------------------------------------------------------------------------
// Volume attachment
// ---------------------------------------------------------------------------

var (
	_ resource.Resource              = (*volumeAttachmentResource)(nil)
	_ resource.ResourceWithConfigure = (*volumeAttachmentResource)(nil)
)

func NewVolumeAttachmentResource() resource.Resource { return &volumeAttachmentResource{} }

type volumeAttachmentResource struct{ client *nemau.Client }

type volumeAttachmentModel struct {
	ID         types.String `tfsdk:"id"`
	VolumeID   types.String `tfsdk:"volume_id"`
	InstanceID types.String `tfsdk:"instance_id"`
	DeviceID   types.String `tfsdk:"device_id"`
}

func (r *volumeAttachmentResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_volume_attachment"
}

func (r *volumeAttachmentResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Attaches a volume to an instance.\n\n" +
			"Destroying it detaches the volume without deleting it: the volume belongs to " +
			"`nemau_volume`.\n\n" +
			"Keep `instance_id` pointing at the `nemau_instance` resource: the platform refuses to " +
			"delete an instance that still has a volume attached, and that reference makes " +
			"Terraform destroy the attachment before the instance.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Synthetic id `<volume_id>:<instance_id>`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"volume_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Id of the volume to attach.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"instance_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Id of the instance. Reference `nemau_instance.<name>.id` so the volume is detached before the instance is destroyed.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"device_id": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Stable device name inside the guest " +
					"(`/dev/disk/by-id/<device_id>`), as reported by the attach task.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *volumeAttachmentResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromResourceProvider(req, resp)
}

func (r *volumeAttachmentResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan volumeAttachmentModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	ref, err := r.client.Storage.AttachVolume(ctx, plan.VolumeID.ValueString(), plan.InstanceID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Attachment was refused", err.Error())
		return
	}
	task, err := r.client.WaitFor(ctx, ref)
	if err != nil {
		resp.Diagnostics.AddError("Attachment failed", taskErrorDetail(err, ref))
		return
	}

	plan.ID = types.StringValue(plan.VolumeID.ValueString() + ":" + plan.InstanceID.ValueString())
	// device_id is only returned in this task result; the volume listing does
	// not include it.
	plan.DeviceID = str(task.ResultString("device_id"))
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *volumeAttachmentResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state volumeAttachmentModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	volume, err := r.client.Storage.GetVolume(ctx, state.VolumeID.ValueString())
	if handleReadError(err, "volume attachment", resp) {
		return
	}
	for _, attachment := range volume.Attachments {
		if attachment.ServerID == state.InstanceID.ValueString() {
			resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
			return
		}
	}
	// Detached outside Terraform: drop it so the next plan reattaches.
	resp.State.RemoveResource(ctx)
}

func (r *volumeAttachmentResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan volumeAttachmentModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *volumeAttachmentResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state volumeAttachmentModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleteAndWait(ctx, r.client, &resp.Diagnostics, "volume attachment", func() (*nemau.TaskRef, error) {
		return r.client.Storage.DetachVolume(ctx, state.VolumeID.ValueString(), state.InstanceID.ValueString())
	})
}

// ---------------------------------------------------------------------------
// Object container
// ---------------------------------------------------------------------------

var (
	_ resource.Resource                   = (*containerResource)(nil)
	_ resource.ResourceWithConfigure      = (*containerResource)(nil)
	_ resource.ResourceWithImportState    = (*containerResource)(nil)
	_ resource.ResourceWithValidateConfig = (*containerResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*containerResource)(nil)
)

func NewContainerResource() resource.Resource { return &containerResource{} }

type containerResource struct{ client *nemau.Client }

type containerModel struct {
	ID            types.String `tfsdk:"id"`
	Name          types.String `tfsdk:"name"`
	Versioning    types.Bool   `tfsdk:"versioning"`
	ObjectLock    types.Bool   `tfsdk:"object_lock"`
	RetentionMode types.String `tfsdk:"retention_mode"`
	RetentionDays types.Int64  `tfsdk:"retention_days"`
	Immutable     types.Bool   `tfsdk:"immutable"`
	Bytes         types.Int64  `tfsdk:"bytes"`
	Count         types.Int64  `tfsdk:"object_count"`
	S3Bucket      types.String `tfsdk:"s3_bucket"`
}

func (r *containerResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_container"
}

func (r *containerResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An object storage container.\n\n" +
			"The provider manages the container but not its contents. Upload objects with " +
			"the CLI or an SDK.\n\n" +
			"`object_lock` makes stored versions immutable for the retention period, as " +
			"ransomware-resistant backups require. S3 cannot add object locking to an existing " +
			"bucket, so `object_lock` and `retention_mode` force a replacement, and a container " +
			"meant to hold immutable backups has to be created with object lock enabled.\n\n" +
			"In `COMPLIANCE` mode, objects cannot be erased before they expire, even by the " +
			"platform, and `terraform destroy` fails while locked versions remain.\n\n" +
			"Only `versioning` and a longer `retention_days` change in place. The API does not " +
			"shorten a retention period, because weakening a WORM guarantee is a support " +
			"operation, and the plan refuses it.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Same as `name`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			// The API cannot rename a container (a PATCH carrying a name is
			// refused), so a new name means a new container.
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Container name. Changing it replaces the container.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			// UseStateForUnknown on every Optional+Computed attribute: on any
			// update the framework marks an omitted computed attribute unknown,
			// and RequiresReplace reads unknown as a change. Without it, toggling
			// versioning on a container that omits object_lock would plan a
			// replacement, which deletes the data.
			"versioning": schema.BoolAttribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Keep previous versions of objects. Implied by " +
					"`object_lock`, and cannot be turned off while it is on. Changes in place.",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"object_lock": schema.BoolAttribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Make versions immutable (WORM). Cannot be changed after creation.",
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.UseStateForUnknown(), boolplanmodifier.RequiresReplace(),
				},
			},
			"retention_mode": schema.StringAttribute{
				Optional: true, Computed: true,
				MarkdownDescription: "`GOVERNANCE` (the default; the platform can lift the lock) " +
					"or `COMPLIANCE` (nobody can lift it before it expires).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(), stringplanmodifier.RequiresReplace(),
				},
			},
			"retention_days": schema.Int64Attribute{
				Optional: true, Computed: true,
				MarkdownDescription: "How long objects stay immutable, in days, at most 2555 (seven " +
					"years). Requires `object_lock`. Can be raised in place, never lowered.",
				PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"immutable": schema.BoolAttribute{
				Computed: true,
				MarkdownDescription: "True when the store enforces WORM. The value is read from " +
					"the store rather than derived from the configuration, so it shows what an " +
					"audit would observe.",
			},
			"bytes": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Bytes stored in the container.",
			},
			"object_count": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Number of objects in the container.",
			},
			"s3_bucket": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Bucket name to address in S3 tooling with a " +
					"`nemau_container_credential` of this container. It differs from `name`: the " +
					"object store knows each container under an opaque name.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *containerResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromResourceProvider(req, resp)
}

func (r *containerResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan containerModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	runAndWait(ctx, r.client, &resp.Diagnostics, "Container creation", func() (*nemau.TaskRef, error) {
		return r.client.Storage.CreateContainerWithOptions(ctx, nemau.CreateContainerRequest{
			Name:          plan.Name.ValueString(),
			Versioning:    plan.Versioning.ValueBool(),
			ObjectLock:    plan.ObjectLock.ValueBool(),
			RetentionMode: plan.RetentionMode.ValueString(),
			RetentionDays: int(plan.RetentionDays.ValueInt64()),
		})
	})
	if resp.Diagnostics.HasError() {
		return
	}
	// The API addresses containers by name, so the name is the id rather than
	// the UUID the task also reports.
	plan.ID = plan.Name
	plan.Bytes = types.Int64Value(0)
	plan.Count = types.Int64Value(0)
	// The bucket name comes from the listing. A failed lookup is not a failed
	// creation: the container exists, and the next refresh fills it in.
	plan.S3Bucket = types.StringValue("")
	if created, err := r.client.Storage.GetContainer(ctx, plan.Name.ValueString()); err == nil && created != nil {
		plan.S3Bucket = types.StringValue(created.S3Bucket)
	}
	r.readProtection(ctx, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *containerResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state containerModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	container, err := r.client.Storage.GetContainer(ctx, state.Name.ValueString())
	if handleReadError(err, "container", resp) {
		return
	}
	state.ID = types.StringValue(container.Name)
	state.Bytes = types.Int64Value(container.Bytes)
	state.Count = types.Int64Value(container.Count)
	state.S3Bucket = types.StringValue(container.S3Bucket)
	r.readProtection(ctx, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update applies what the API lets a container change in place: versioning,
// and a longer retention. The name, object_lock and retention_mode force a
// replacement, so they never reach here changed.
//
// The new state is built from what the platform reports after the change, and
// is not copied from the plan. An audit reads a container's protection from
// the state, so the state must not record a retention the store does not hold.
func (r *containerResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state containerModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()
	next := state

	update := containerUpdate(plan, state)
	applied := false
	if update != (nemau.Update{}) {
		var diags diag.Diagnostics
		runAndWait(ctx, r.client, &diags, "Container update", func() (*nemau.TaskRef, error) {
			return r.client.Storage.UpdateContainer(ctx, name, update)
		})
		resp.Diagnostics.Append(diags...)
		applied = !diags.HasError()
		if applied {
			// The completed task is the platform's confirmation. The live read
			// below replaces these values when it succeeds.
			if update.Versioning != nil {
				next.Versioning = types.BoolValue(*update.Versioning)
			}
			if update.RetentionDays != nil {
				next.RetentionDays = types.Int64Value(int64(*update.RetentionDays))
			}
		}
	}

	if found, err := r.client.Storage.GetContainer(ctx, name); err == nil && found != nil {
		next.Bytes = types.Int64Value(found.Bytes)
		next.Count = types.Int64Value(found.Count)
		next.S3Bucket = types.StringValue(found.S3Bucket)
	}
	// A state written before s3_bucket existed plans it as unknown; a computed
	// value is never left unknown after apply.
	settleUnknownString(&next.S3Bucket, "")
	settleUnknownInt64(&next.Bytes, 0)
	settleUnknownInt64(&next.Count, 0)

	if protection, err := r.client.Storage.ContainerProtection(ctx, name); err == nil && protection != nil {
		applyProtection(&next, protection)
		if applied {
			checkContainerUpdateLanded(&resp.Diagnostics, update, protection)
		}
	}
	settleUnknownBool(&next.Immutable, false)
	resp.Diagnostics.Append(resp.State.Set(ctx, &next)...)
}

// containerUpdate is the PATCH that takes a container from state to plan:
// only the fields the API accepts, and only those that change. An unknown
// planned value is not a change.
func containerUpdate(plan, state containerModel) nemau.Update {
	var update nemau.Update
	if !plan.Versioning.IsUnknown() && !plan.Versioning.IsNull() &&
		!plan.Versioning.Equal(state.Versioning) {
		update.Versioning = nemau.Set(plan.Versioning.ValueBool())
	}
	if !plan.RetentionDays.IsUnknown() && !plan.RetentionDays.IsNull() &&
		!plan.RetentionDays.Equal(state.RetentionDays) {
		update.RetentionDays = nemau.Set(int(plan.RetentionDays.ValueInt64()))
	}
	return update
}

// checkContainerUpdateLanded turns a completed update the store does not
// reflect into an error, so the apply does not report a protection that the
// container does not have.
func checkContainerUpdateLanded(diags *diag.Diagnostics, update nemau.Update, protection *nemau.ContainerProtection) {
	if update.Versioning != nil && *update.Versioning != (protection.Versioning == "Enabled") {
		diags.AddAttributeError(tfpathRoot("versioning"), "Container update not applied",
			fmt.Sprintf("The update completed, but the object store reports versioning %q. "+
				"The state records what the store reports.", protection.Versioning))
	}
	if update.RetentionDays != nil && *update.RetentionDays != protection.RetentionDays {
		diags.AddAttributeError(tfpathRoot("retention_days"), "Container update not applied",
			fmt.Sprintf("The update completed, but the object store reports a retention of %d days, "+
				"not %d. The state records what the store reports.",
				protection.RetentionDays, *update.RetentionDays))
	}
}

func (r *containerResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state containerModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleteAndWait(ctx, r.client, &resp.Diagnostics, "container", func() (*nemau.TaskRef, error) {
		return r.client.Storage.DeleteContainer(ctx, state.Name.ValueString())
	})
}

func (r *containerResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// A container is addressed by name and Read looks it up by name, so import
	// has to set both attributes. Setting only `id`, as the generic helper
	// does, would leave `name` null and make the first Read remove the imported
	// resource.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, tfpathRoot("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, tfpathRoot("name"), req.ID)...)
}

// maxRetentionDays is the longest default retention the API accepts. A
// COMPLIANCE lock cannot be lifted, so the server refuses more than this.
const maxRetentionDays = 2555

// ModifyPlan refuses at plan time what the API would refuse at apply, halfway
// through a run that may already have changed other resources: a shorter
// retention.
func (r *containerResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return // create or destroy: nothing to compare
	}
	var plan, state containerModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if plan.RetentionDays.IsUnknown() || plan.RetentionDays.IsNull() ||
		state.RetentionDays.IsUnknown() || state.RetentionDays.IsNull() {
		return
	}
	if planned, current := plan.RetentionDays.ValueInt64(), state.RetentionDays.ValueInt64(); planned < current {
		resp.Diagnostics.AddAttributeError(tfpathRoot("retention_days"), "Retention cannot be shortened",
			fmt.Sprintf("retention_days can only be raised: %d is below the current %d days. "+
				"Shortening the retention of a WORM container is a support operation and cannot "+
				"be done through the API; contact support if it is needed. To keep the current value, "+
				"set retention_days = %d.", planned, current, current))
	}
}

// ValidateConfig catches a retention policy declared without object lock at
// plan time, where the message can name the attribute the practitioner wrote.
func (r *containerResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config containerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	locked := config.ObjectLock.ValueBool()

	if !locked && !config.ObjectLock.IsUnknown() {
		if !config.RetentionDays.IsNull() || !config.RetentionMode.IsNull() {
			resp.Diagnostics.AddError("Retention without object lock",
				"`retention_days` and `retention_mode` require `object_lock = true`. "+
					"Without the lock, nothing holds a deleted version.")
		}
	}
	// The API turns versioning on with object lock, and refuses to suspend it
	// while the lock is on: a lock over unversioned objects holds nothing.
	if locked && !config.Versioning.IsNull() && !config.Versioning.IsUnknown() && !config.Versioning.ValueBool() {
		resp.Diagnostics.AddAttributeError(tfpathRoot("versioning"), "Versioning required by object lock",
			"`object_lock = true` keeps versioning on. Remove `versioning = false`, or set it to true.")
	}
	if mode := config.RetentionMode.ValueString(); mode != "" &&
		mode != nemau.RetentionGovernance && mode != nemau.RetentionCompliance {
		resp.Diagnostics.AddAttributeError(tfpathRoot("retention_mode"), "Unknown retention mode",
			"retention_mode must be \""+nemau.RetentionGovernance+"\" or \""+
				nemau.RetentionCompliance+"\".")
	}
	if days := config.RetentionDays; !days.IsNull() && !days.IsUnknown() && days.ValueInt64() > maxRetentionDays {
		resp.Diagnostics.AddAttributeError(tfpathRoot("retention_days"), "Retention too long",
			fmt.Sprintf("retention_days must not exceed %d (seven years).", maxRetentionDays))
	}
	// Object lock without a duration relies on a per-object retention this
	// provider does not set, so nothing would actually be held.
	if locked && config.RetentionDays.IsNull() {
		resp.Diagnostics.AddAttributeWarning(tfpathRoot("retention_days"),
			"Object lock with no retention period",
			"`object_lock` without `retention_days` turns the lock on but holds no object "+
				"by default: versions stay erasable.")
	}
}

// readProtection fills the protection attributes from what the store enforces.
//
// The values are read live rather than copied from the plan: an audit relies
// on `immutable`, which must reflect the store and not the configuration.
func (r *containerResource) readProtection(ctx context.Context, model *containerModel) {
	protection, err := r.client.Storage.ContainerProtection(ctx, model.Name.ValueString())
	if err != nil || protection == nil {
		// The creation task completed, so the container exists and a failed read
		// must not fail the apply. Settle every computed attribute instead,
		// because Terraform errors on an unknown value left in state.
		settleUnknownBool(&model.Versioning, false)
		settleUnknownBool(&model.ObjectLock, false)
		settleUnknownString(&model.RetentionMode, "")
		settleUnknownInt64(&model.RetentionDays, 0)
		model.Immutable = types.BoolValue(false)
		return
	}
	applyProtection(model, protection)
}

// applyProtection copies a live protection read into the model.
func applyProtection(model *containerModel, protection *nemau.ContainerProtection) {
	model.Versioning = types.BoolValue(protection.Versioning == "Enabled")
	model.ObjectLock = types.BoolValue(protection.ObjectLock)
	model.RetentionMode = str(protection.RetentionMode)
	if model.RetentionMode.IsNull() {
		model.RetentionMode = types.StringValue("")
	}
	model.RetentionDays = types.Int64Value(int64(protection.RetentionDays))
	model.Immutable = types.BoolValue(protection.Immutable())
}
