package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

var (
	_ resource.Resource                = (*networkResource)(nil)
	_ resource.ResourceWithConfigure   = (*networkResource)(nil)
	_ resource.ResourceWithImportState = (*networkResource)(nil)
)

func NewNetworkResource() resource.Resource { return &networkResource{} }

type networkResource struct{ client *nemau.Client }

type networkModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	Status      types.String `tfsdk:"status"`
	External    types.Bool   `tfsdk:"external"`
}

func (r *networkResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_network"
}

func (r *networkResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An isolated virtual network in the project.\n\n" +
			"`name` and `description` change in place: the segment keeps its identifier, so " +
			"the subnets and instances attached to it are untouched.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Network id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Network name. Changes in place.",
			},
			"description": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Free-form description. Changes in place.",
			},
			"status": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Status as reported by the platform.",
			},
			"external": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "True for the region's external network, never for one created here.",
			},
		},
	}
}

func (r *networkResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromResourceProvider(req, resp)
}

func (r *networkResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan networkModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id := createAndWait(ctx, r.client, &resp.Diagnostics, "network", func() (*nemau.TaskRef, error) {
		return r.client.Network.CreateNetwork(ctx, plan.Name.ValueString(), plan.Description.ValueString())
	}, "network_id")
	if resp.Diagnostics.HasError() {
		return
	}

	plan.ID = types.StringValue(id)
	r.refresh(ctx, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *networkResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state networkModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	network, err := r.client.Network.GetNetwork(ctx, state.ID.ValueString())
	if handleReadError(err, "network", resp) {
		return
	}
	applyNetwork(&state, network)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update changes the network's name or description in place.
//
// Neither change touches the network segment: its identifier is fixed at
// creation and the display name is a separate field, so a rename leaves every
// attached subnet and instance in place.
func (r *networkResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state networkModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	update := nemau.Update{}
	// Only what actually changed is sent: a PATCH carrying every attribute
	// would overwrite a field another writer touched between the plan and the
	// apply.
	if !plan.Name.Equal(state.Name) {
		update.Name = nemau.Set(plan.Name.ValueString())
	}
	if !plan.Description.Equal(state.Description) {
		update.Description = nemau.Set(plan.Description.ValueString())
	}

	id := state.ID.ValueString()
	runAndWait(ctx, r.client, &resp.Diagnostics, "network update", func() (*nemau.TaskRef, error) {
		return r.client.Network.UpdateNetwork(ctx, id, update)
	})
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = state.ID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *networkResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state networkModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleteAndWaitWithHint(ctx, r.client, &resp.Diagnostics, "network",
		"Something still sits on this network (a subnet, an instance or a load balancer). Terraform "+
			"destroys dependants first when they reference `nemau_network.<name>.id`; remove the ones "+
			"created outside Terraform, then apply again.",
		func() (*nemau.TaskRef, error) {
			return r.client.Network.DeleteNetwork(ctx, state.ID.ValueString())
		})
}

func (r *networkResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importByID(ctx, req, resp)
}

func (r *networkResource) refresh(ctx context.Context, model *networkModel) {
	network, err := r.client.Network.GetNetwork(ctx, model.ID.ValueString())
	if err != nil {
		// The create task completed, so the resource exists. A failed refresh
		// must not fail the apply and orphan it; fill in what the plan knows.
		model.Status = types.StringValue("unknown")
		model.External = types.BoolValue(false)
		return
	}
	applyNetwork(model, network)
}

func applyNetwork(model *networkModel, network *nemau.Network) {
	model.ID = types.StringValue(network.ID)
	model.Name = types.StringValue(network.Name)
	model.Description = str(network.Description)
	model.Status = str(network.Status)
	model.External = types.BoolValue(network.External)
}
