package provider

import (
	"context"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

var (
	_ resource.Resource                   = (*instanceResource)(nil)
	_ resource.ResourceWithConfigure      = (*instanceResource)(nil)
	_ resource.ResourceWithImportState    = (*instanceResource)(nil)
	_ resource.ResourceWithValidateConfig = (*instanceResource)(nil)
)

func NewInstanceResource() resource.Resource { return &instanceResource{} }

type instanceResource struct{ client *nemau.Client }

type instanceModel struct {
	ID             types.String `tfsdk:"id"`
	Name           types.String `tfsdk:"name"`
	Description    types.String `tfsdk:"description"`
	ImageID        types.String `tfsdk:"image_id"`
	FlavorID       types.String `tfsdk:"flavor_id"`
	VCPUs          types.Int64  `tfsdk:"vcpus"`
	RAMMB          types.Int64  `tfsdk:"ram_mb"`
	DiskGB         types.Int64  `tfsdk:"disk_gb"`
	ServiceLevel   types.String `tfsdk:"service_level"`
	NetworkID      types.String `tfsdk:"network_id"`
	SecurityGroups types.Set    `tfsdk:"security_group_ids"`
	KeyName        types.String `tfsdk:"key_name"`
	Started        types.Bool   `tfsdk:"started"`
	Status         types.String `tfsdk:"status"`
	Addresses      types.List   `tfsdk:"addresses"`
}

func (r *instanceResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_instance"
}

func (r *instanceResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A compute instance.\n\n" +
			"The size is either `flavor_id` or the three `vcpus`, `ram_mb` and `disk_gb` " +
			"values: the API rejects a partial custom size. `flavor_id` additionally requires " +
			"the `compute-flavors` organization feature.\n\n" +
			"An instance cannot be destroyed while a volume is attached: the platform refuses, " +
			"because the volume's disk would be destroyed with it. Reference the instance from " +
			"the `nemau_volume_attachment` (`instance_id = nemau_instance.<name>.id`) and Terraform " +
			"detaches the volume first.\n\n" +
			"The size (`vcpus`, `ram_mb`, `disk_gb`), the security groups and the power state " +
			"(`started`) change in place. The image, flavor, network, service level and SSH key " +
			"force a replacement.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Instance id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Instance name. Changes in place.",
			},
			"description": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Free-form description. Changes in place.",
			},
			"image_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Image to boot. See the `nemau_images` data source.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"flavor_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Flavor id. Mutually exclusive with the custom size. See the `nemau_flavors` data source.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"vcpus": schema.Int64Attribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Number of vCPUs (1-64). Changes in place, but the instance is " +
					"power-cycled to apply it.",
			},
			"ram_mb": schema.Int64Attribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Memory in MB (1024-262144). Changes in place, but the instance is " +
					"power-cycled to apply it.",
			},
			"disk_gb": schema.Int64Attribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Disk in GB (10-5000). Grown online: raising it alone leaves the " +
					"instance running. A disk never shrinks.",
			},
			"service_level": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "`99.9%` (covered by the availability SLA, the default) or " +
					"`best-effort` (no SLA, billed at a lower rate). Changing it replaces the instance.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"network_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Id of the network to attach at boot.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"security_group_ids": schema.SetAttribute{
				Optional:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Security groups applied to the instance. A change replaces " +
					"the existing groups; an empty set removes all filtering without destroying " +
					"the instance.",
			},
			"key_name": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Name of the SSH keypair injected at boot.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"started": schema.BoolAttribute{
				Optional: true, Computed: true,
				Default:             booldefault.StaticBool(true),
				MarkdownDescription: "Desired power state. Setting `false` stops the instance without destroying it.",
			},
			"status": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Reported status: running, stopped, paused or pending.",
			},
			"addresses": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Known IP addresses. Empty until the guest has reported its " +
					"network configuration.",
			},
		},
	}
}

func (r *instanceResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromResourceProvider(req, resp)
}

// ValidateConfig checks the sizing rule at plan time. The API enforces it too,
// but checking it here stops the run before `terraform apply` has created
// other resources.
func (r *instanceResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config instanceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !config.FlavorID.IsNull() && !config.FlavorID.IsUnknown() {
		return
	}
	// Unknown values come from another resource and cannot be checked yet.
	for _, value := range []types.Int64{config.VCPUs, config.RAMMB, config.DiskGB} {
		if value.IsUnknown() {
			return
		}
	}
	if config.VCPUs.IsNull() || config.RAMMB.IsNull() || config.DiskGB.IsNull() {
		resp.Diagnostics.AddError("Incomplete instance size",
			"Set `flavor_id`, or all three of `vcpus`, `ram_mb` and `disk_gb`. "+
				"The API rejects a partial custom size.")
	}
}

func (r *instanceResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan instanceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	groups := setToStrings(ctx, plan.SecurityGroups, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	id := createAndWait(ctx, r.client, &resp.Diagnostics, "instance", func() (*nemau.TaskRef, error) {
		return r.client.Compute.CreateInstance(ctx, nemau.CreateInstanceRequest{
			Name:           plan.Name.ValueString(),
			Description:    plan.Description.ValueString(),
			ImageID:        plan.ImageID.ValueString(),
			FlavorID:       plan.FlavorID.ValueString(),
			VCPUs:          int(plan.VCPUs.ValueInt64()),
			RAMMB:          int(plan.RAMMB.ValueInt64()),
			DiskGB:         int(plan.DiskGB.ValueInt64()),
			ServiceLevel:   plan.ServiceLevel.ValueString(),
			NetworkID:      plan.NetworkID.ValueString(),
			SecurityGroups: groups,
			KeyName:        plan.KeyName.ValueString(),
		})
	}, "instance_id")
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(id)

	// An instance boots started. Only an explicit `started = false` needs an
	// extra call.
	if !plan.Started.ValueBool() {
		runAndWait(ctx, r.client, &resp.Diagnostics, "Stopping the instance", func() (*nemau.TaskRef, error) {
			return r.client.Compute.Stop(ctx, id)
		})
		if resp.Diagnostics.HasError() {
			return
		}
	}

	r.refresh(ctx, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *instanceResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state instanceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	instance, err := r.client.Compute.GetInstance(ctx, state.ID.ValueString())
	if handleReadError(err, "instance", resp) {
		return
	}
	applyInstance(&state, instance)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *instanceResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state instanceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()
	plan.ID = state.ID

	// Resize first: it is the only change that can fail for lack of capacity,
	// and running it before the others avoids a failed apply that has already
	// applied part of the changes.
	if plan.VCPUs != state.VCPUs || plan.RAMMB != state.RAMMB || plan.DiskGB != state.DiskGB {
		runAndWait(ctx, r.client, &resp.Diagnostics, "Resize", func() (*nemau.TaskRef, error) {
			return r.client.Compute.Resize(ctx, id, nemau.Sizing{
				VCPUs:  int(plan.VCPUs.ValueInt64()),
				RAMMB:  int(plan.RAMMB.ValueInt64()),
				DiskGB: int(plan.DiskGB.ValueInt64()),
			})
		})
		if resp.Diagnostics.HasError() {
			return
		}
	}

	// Only the fields that changed are sent, as for the other resources.
	update := nemau.Update{}
	if !plan.Name.Equal(state.Name) {
		update.Name = nemau.Set(plan.Name.ValueString())
	}
	if !plan.Description.Equal(state.Description) {
		update.Description = nemau.Set(plan.Description.ValueString())
	}
	if update != (nemau.Update{}) {
		runAndWait(ctx, r.client, &resp.Diagnostics, "Instance update", func() (*nemau.TaskRef, error) {
			return r.client.Compute.UpdateInstance(ctx, id, update)
		})
		if resp.Diagnostics.HasError() {
			return
		}
	}

	if !plan.SecurityGroups.Equal(state.SecurityGroups) {
		groups := setToStrings(ctx, plan.SecurityGroups, &resp.Diagnostics)
		if resp.Diagnostics.HasError() {
			return
		}
		runAndWait(ctx, r.client, &resp.Diagnostics, "Security-group change", func() (*nemau.TaskRef, error) {
			return r.client.Compute.SetSecurityGroups(ctx, id, groups)
		})
		if resp.Diagnostics.HasError() {
			return
		}
	}

	if plan.Started.ValueBool() != state.Started.ValueBool() {
		action := r.client.Compute.Start
		label := "Starting the instance"
		if !plan.Started.ValueBool() {
			action, label = r.client.Compute.Stop, "Stopping the instance"
		}
		runAndWait(ctx, r.client, &resp.Diagnostics, label, func() (*nemau.TaskRef, error) {
			return action(ctx, id)
		})
		if resp.Diagnostics.HasError() {
			return
		}
	}

	r.refresh(ctx, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *instanceResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state instanceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleteAndWaitWithHint(ctx, r.client, &resp.Diagnostics, "instance",
		"A volume is still attached to this instance. Destroy its `nemau_volume_attachment` first "+
			"(Terraform does this on its own when the attachment references `nemau_instance.<name>.id`), "+
			"or detach it outside Terraform with `nemau storage volume detach`, then apply again.",
		func() (*nemau.TaskRef, error) {
			return r.client.Compute.DeleteInstance(ctx, state.ID.ValueString())
		})
}

func (r *instanceResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importByID(ctx, req, resp)
}

// refresh fills computed attributes after a write.
//
// A read failure here is not fatal: the resource exists, and failing the apply
// would leave it running with no state entry. The computed fields are filled
// with what is knowable instead.
func (r *instanceResource) refresh(ctx context.Context, model *instanceModel) {
	instance, err := r.client.Compute.GetInstance(ctx, model.ID.ValueString())
	if err != nil {
		model.Status = types.StringValue("unknown")
		model.Addresses = types.ListValueMust(types.StringType, []attr.Value{})
		return
	}
	applyInstance(model, instance)
}

func applyInstance(model *instanceModel, instance *nemau.Instance) {
	model.ID = types.StringValue(instance.ID)
	model.Name = types.StringValue(instance.Name)
	model.Description = str(instance.Description)
	model.Status = str(instance.Status)
	model.Addresses = strList(instance.Addresses)
	model.VCPUs = types.Int64Value(int64(instance.Sizing.VCPUs))
	model.RAMMB = types.Int64Value(int64(instance.Sizing.RAMMB))
	model.DiskGB = types.Int64Value(int64(instance.Sizing.DiskGB))
	if instance.KeyName != "" {
		model.KeyName = types.StringValue(instance.KeyName)
	}
	if instance.Image != nil && instance.Image.ID != "" {
		model.ImageID = types.StringValue(instance.Image.ID)
	}
	if instance.Flavor != nil && instance.Flavor.ID != "" {
		model.FlavorID = types.StringValue(instance.Flavor.ID)
	}
	// Only "running" counts as started. A paused or suspended instance is
	// neither running nor cleanly stopped, and reporting it as started would
	// hide the drift.
	model.Started = types.BoolValue(instance.Status == "running")
}
