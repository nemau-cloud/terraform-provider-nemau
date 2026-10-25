package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

// ---------------------------------------------------------------------------
// Subnet
// ---------------------------------------------------------------------------

var (
	_ resource.Resource                   = (*subnetResource)(nil)
	_ resource.ResourceWithConfigure      = (*subnetResource)(nil)
	_ resource.ResourceWithImportState    = (*subnetResource)(nil)
	_ resource.ResourceWithValidateConfig = (*subnetResource)(nil)
)

func NewSubnetResource() resource.Resource { return &subnetResource{} }

type subnetResource struct{ client *nemau.Client }

type subnetModel struct {
	ID         types.String `tfsdk:"id"`
	NetworkID  types.String `tfsdk:"network_id"`
	Name       types.String `tfsdk:"name"`
	IPVersion  types.Int64  `tfsdk:"ip_version"`
	CIDR       types.String `tfsdk:"cidr"`
	GatewayIP  types.String `tfsdk:"gateway_ip"`
	DNSServers types.List   `tfsdk:"dns_servers"`
}

func (r *subnetResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_subnet"
}

func (r *subnetResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An address range inside a network.\n\n" +
			"`name` changes in place. Changing the range or the gateway replaces the " +
			"subnet.\n\n" +
			"An IPv6 subnet (`ip_version = 6`) takes no `cidr` and no `gateway_ip`: the platform " +
			"assigns a /64 from the region's routed prefix and its first address as the gateway, " +
			"both readable once created. The network must already have an IPv4 subnet, and " +
			"instances created on it afterwards get a public IPv6 address and need a security group.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Subnet id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"network_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Id of the network the subnet belongs to. Changing it replaces the subnet.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Subnet name. Changes in place.",
			},
			"ip_version": schema.Int64Attribute{
				Optional: true, Computed: true,
				MarkdownDescription: "`4` (the default) or `6`.",
				// UseStateForUnknown comes first so that an omitted version keeps
				// the value the API reported instead of forcing a replacement.
				PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown(), int64RequiresReplace()},
			},
			"cidr": schema.StringAttribute{
				Optional: true, Computed: true,
				MarkdownDescription: "CIDR range, for example `10.0.1.0/24`. Required for IPv4. For IPv6 the " +
					"platform assigns it, and it must not be set. An IPv4 range must not " +
					"overlap any other subnet of the project, on this network or another: the subnets " +
					"of a project are routed together.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown(), stringplanmodifier.RequiresReplace()},
			},
			// The computed attributes below keep their state value when omitted:
			// without UseStateForUnknown a rename marks them unknown, and
			// RequiresReplace then reads that as a change and recreates the subnet.
			"gateway_ip": schema.StringAttribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Gateway address. When omitted, the platform picks one. IPv4 only.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown(), stringplanmodifier.RequiresReplace()},
			},
			"dns_servers": schema.ListAttribute{
				Optional: true, Computed: true,
				ElementType: types.StringType,
				MarkdownDescription: "DNS resolvers announced to instances on the subnet. When omitted, " +
					"the platform chooses them. Changing them replaces the subnet.",
				PlanModifiers: []planmodifier.List{listplanmodifier.UseStateForUnknown(), listplanmodifier.RequiresReplace()},
			},
		},
	}
}

func (r *subnetResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromResourceProvider(req, resp)
}

// ValidateConfig checks the IPv4/IPv6 shape at plan time. The API refuses the
// same things, but only once `terraform apply` is already creating resources.
func (r *subnetResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config subnetModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() || config.IPVersion.IsUnknown() {
		return
	}
	version := int64(nemau.IPv4)
	if !config.IPVersion.IsNull() {
		version = config.IPVersion.ValueInt64()
	}
	switch version {
	case nemau.IPv4:
		if config.CIDR.IsNull() {
			resp.Diagnostics.AddAttributeError(tfpathRoot("cidr"), "Missing CIDR",
				"An IPv4 subnet needs `cidr`, for example `10.0.1.0/24`.")
		}
	case nemau.IPv6:
		for _, attribute := range []struct {
			name  string
			value types.String
		}{{"cidr", config.CIDR}, {"gateway_ip", config.GatewayIP}} {
			if !attribute.value.IsNull() {
				resp.Diagnostics.AddAttributeError(tfpathRoot(attribute.name), "Not settable on an IPv6 subnet",
					"The platform assigns an IPv6 subnet's /64 and its gateway. Remove `"+attribute.name+"`.")
			}
		}
	default:
		resp.Diagnostics.AddAttributeError(tfpathRoot("ip_version"), "Invalid IP version",
			"`ip_version` must be 4 or 6.")
	}
}

func (r *subnetResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan subnetModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	request := nemau.CreateSubnetRequest{
		Name:       plan.Name.ValueString(),
		DNSServers: stringsFromList(ctx, plan.DNSServers, &resp.Diagnostics),
	}
	if !plan.CIDR.IsNull() && !plan.CIDR.IsUnknown() {
		request.CIDR = plan.CIDR.ValueString()
	}
	if !plan.IPVersion.IsNull() && !plan.IPVersion.IsUnknown() {
		request.IPVersion = int(plan.IPVersion.ValueInt64())
	}
	// A pointer, because an explicitly empty gateway (a subnet without one) is
	// not the same request as "let the platform choose".
	if !plan.GatewayIP.IsNull() && !plan.GatewayIP.IsUnknown() {
		gateway := plan.GatewayIP.ValueString()
		request.GatewayIP = &gateway
	}
	if resp.Diagnostics.HasError() {
		return
	}

	ref, err := r.client.Network.CreateSubnet(ctx, plan.NetworkID.ValueString(), request)
	if detail, overlaps := subnetOverlapDetail(err); overlaps {
		resp.Diagnostics.AddAttributeError(tfpathRoot("cidr"), "Subnet range already in use", detail)
		return
	}
	id := createAndWait(ctx, r.client, &resp.Diagnostics, "subnet", func() (*nemau.TaskRef, error) {
		return ref, err
	}, "subnet_id")
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(id)
	if subnet, err := r.client.Network.GetSubnet(ctx, id); err == nil {
		applySubnet(&plan, subnet)
	} else {
		plan.GatewayIP = str(plan.GatewayIP.ValueString())
		plan.DNSServers = strList(stringsFromList(ctx, plan.DNSServers, &resp.Diagnostics))
		settleUnknownString(&plan.CIDR, "")
		settleUnknownInt64(&plan.IPVersion, nemau.IPv4)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// subnetOverlapDetail explains the API's refusal of an overlapping range, for
// a diagnostic attached to `cidr` rather than to the whole resource. The API's
// message names the conflicting subnet. The added sentence covers a case
// specific to Terraform, where the configuration looks correct: a replacement
// under create_before_destroy.
func subnetOverlapDetail(err error) (string, bool) {
	var apiErr *nemau.APIError
	if !nemau.IsSubnetOverlap(err) || !errors.As(err, &apiErr) {
		return "", false
	}
	return apiErr.Message + ".\n\nIf this subnet replaces another under create_before_destroy, the old " +
		"one still holds its range while the new one is created: give the replacement a free range.", true
}

func (r *subnetResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state subnetModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	subnet, err := r.client.Network.GetSubnet(ctx, state.ID.ValueString())
	if handleReadError(err, "subnet", resp) {
		return
	}
	applySubnet(&state, subnet)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *subnetResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state subnetModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	update := nemau.Update{}
	// Only what actually changed is sent: a PATCH carrying every attribute
	// would overwrite a field another writer touched between the plan and
	// the apply.
	if !plan.Name.Equal(state.Name) {
		update.Name = nemau.Set(plan.Name.ValueString())
	}

	id := state.ID.ValueString()
	runAndWait(ctx, r.client, &resp.Diagnostics, "subnet update", func() (*nemau.TaskRef, error) {
		return r.client.Network.UpdateSubnet(ctx, id, update)
	})
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = state.ID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *subnetResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state subnetModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleteAndWait(ctx, r.client, &resp.Diagnostics, "subnet", func() (*nemau.TaskRef, error) {
		return r.client.Network.DeleteSubnet(ctx, state.ID.ValueString())
	})
}

func (r *subnetResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importByID(ctx, req, resp)
}

func applySubnet(model *subnetModel, subnet *nemau.Subnet) {
	model.ID = types.StringValue(subnet.ID)
	model.Name = types.StringValue(subnet.Name)
	model.NetworkID = types.StringValue(subnet.NetworkID)
	model.IPVersion = types.Int64Value(int64(ipVersionOrDefault(subnet.IPVersion)))
	model.CIDR = types.StringValue(subnet.CIDR)
	model.GatewayIP = str(subnet.GatewayIP)
	model.DNSServers = strList(subnet.DNSServers)
}

// ipVersionOrDefault reads a version from the API. A region without IPv6
// omits the field, and everything it serves is IPv4.
func ipVersionOrDefault(version int) int {
	if version == 0 {
		return nemau.IPv4
	}
	return version
}

// ---------------------------------------------------------------------------
// Floating IP
// ---------------------------------------------------------------------------

var (
	_ resource.Resource                   = (*floatingIPResource)(nil)
	_ resource.ResourceWithConfigure      = (*floatingIPResource)(nil)
	_ resource.ResourceWithImportState    = (*floatingIPResource)(nil)
	_ resource.ResourceWithValidateConfig = (*floatingIPResource)(nil)
)

func NewFloatingIPResource() resource.Resource { return &floatingIPResource{} }

type floatingIPResource struct{ client *nemau.Client }

type floatingIPModel struct {
	ID                types.String `tfsdk:"id"`
	Description       types.String `tfsdk:"description"`
	ExternalNetworkID types.String `tfsdk:"external_network_id"`
	IPVersion         types.Int64  `tfsdk:"ip_version"`
	Address           types.String `tfsdk:"address"`
	Status            types.String `tfsdk:"status"`
}

func (r *floatingIPResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_floating_ip"
}

func (r *floatingIPResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A public address allocated to the project.\n\n" +
			"Allocation and association are separate resources, as in the API (see " +
			"`nemau_floating_ip_association`). This lets you keep the address while " +
			"replacing the machine behind it.\n\n" +
			"An IPv6 floating IP (`ip_version = 6`) is translated to the target's private IPv4 " +
			"address on the region's exit node: the guest needs no IPv6 configuration and sees " +
			"IPv6 clients as addresses of the translation range. A target holds at most one.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Floating IP id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"description": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Free-form description. Changes in place.",
			},
			"external_network_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Id of the external network to allocate from. When omitted, the " +
					"region's external network is used. Changing it replaces the floating IP.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"ip_version": schema.Int64Attribute{
				Optional: true, Computed: true,
				MarkdownDescription: "`4` (the default) or `6`.",
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown(), int64RequiresReplace()},
			},
			"address": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The allocated IP address.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"status": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Status as reported by the platform.",
			},
		},
	}
}

func (r *floatingIPResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromResourceProvider(req, resp)
}

func (r *floatingIPResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config floatingIPModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() || config.IPVersion.IsNull() || config.IPVersion.IsUnknown() {
		return
	}
	if version := config.IPVersion.ValueInt64(); version != nemau.IPv4 && version != nemau.IPv6 {
		resp.Diagnostics.AddAttributeError(tfpathRoot("ip_version"), "Invalid IP version",
			"`ip_version` must be 4 or 6.")
	}
}

func (r *floatingIPResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan floatingIPModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	request := nemau.AllocateFloatingIPRequest{
		FloatingNetworkID: plan.ExternalNetworkID.ValueString(),
		Description:       plan.Description.ValueString(),
	}
	if !plan.IPVersion.IsNull() && !plan.IPVersion.IsUnknown() {
		request.IPVersion = int(plan.IPVersion.ValueInt64())
	}
	id := createAndWait(ctx, r.client, &resp.Diagnostics, "floating IP", func() (*nemau.TaskRef, error) {
		return r.client.Network.AllocateFloatingIPWith(ctx, request)
	}, "floating_ip_id")
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(id)
	if fip, err := r.client.Network.GetFloatingIP(ctx, id); err == nil {
		applyFloatingIP(&plan, fip)
	} else {
		plan.Address = types.StringValue("")
		plan.Status = types.StringValue("unknown")
		settleUnknownInt64(&plan.IPVersion, nemau.IPv4)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *floatingIPResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state floatingIPModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	fip, err := r.client.Network.GetFloatingIP(ctx, state.ID.ValueString())
	if handleReadError(err, "floating IP", resp) {
		return
	}
	applyFloatingIP(&state, fip)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *floatingIPResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state floatingIPModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	update := nemau.Update{}
	// Only what actually changed is sent: a PATCH carrying every attribute
	// would overwrite a field another writer touched between the plan and
	// the apply.
	if !plan.Description.Equal(state.Description) {
		update.Description = nemau.Set(plan.Description.ValueString())
	}

	id := state.ID.ValueString()
	runAndWait(ctx, r.client, &resp.Diagnostics, "floating IP update", func() (*nemau.TaskRef, error) {
		return r.client.Network.UpdateFloatingIP(ctx, id, update)
	})
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = state.ID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *floatingIPResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state floatingIPModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleteAndWait(ctx, r.client, &resp.Diagnostics, "floating IP", func() (*nemau.TaskRef, error) {
		return r.client.Network.ReleaseFloatingIP(ctx, state.ID.ValueString())
	})
}

func (r *floatingIPResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importByID(ctx, req, resp)
}

func applyFloatingIP(model *floatingIPModel, fip *nemau.FloatingIP) {
	model.ID = types.StringValue(fip.ID)
	model.IPVersion = types.Int64Value(int64(ipVersionOrDefault(fip.IPVersion)))
	model.Address = types.StringValue(fip.Address)
	model.Status = str(fip.Status)
	model.Description = str(fip.Description)
	if fip.ExternalNetworkID != "" {
		model.ExternalNetworkID = types.StringValue(fip.ExternalNetworkID)
	}
}

// ---------------------------------------------------------------------------
// Floating IP association
// ---------------------------------------------------------------------------

var (
	_ resource.Resource                   = (*floatingIPAssociationResource)(nil)
	_ resource.ResourceWithConfigure      = (*floatingIPAssociationResource)(nil)
	_ resource.ResourceWithValidateConfig = (*floatingIPAssociationResource)(nil)
)

func NewFloatingIPAssociationResource() resource.Resource { return &floatingIPAssociationResource{} }

type floatingIPAssociationResource struct{ client *nemau.Client }

type floatingIPAssociationModel struct {
	ID           types.String `tfsdk:"id"`
	FloatingIPID types.String `tfsdk:"floating_ip_id"`
	TargetType   types.String `tfsdk:"target_type"`
	TargetID     types.String `tfsdk:"target_id"`
}

func (r *floatingIPAssociationResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_floating_ip_association"
}

func (r *floatingIPAssociationResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Attaches a floating IP to an instance or a load balancer.\n\n" +
			"Destroying it dissociates the address without releasing it: the allocation belongs " +
			"to `nemau_floating_ip`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Synthetic id `<floating_ip_id>`. An address has only one " +
					"target at a time.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"floating_ip_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Id of the `nemau_floating_ip` to attach.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"target_type": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "`instance` or `load_balancer`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"target_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Id of the instance or load balancer that receives the address.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
		},
	}
}

func (r *floatingIPAssociationResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromResourceProvider(req, resp)
}

func (r *floatingIPAssociationResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config floatingIPAssociationModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() || config.TargetType.IsUnknown() || config.TargetType.IsNull() {
		return
	}
	switch config.TargetType.ValueString() {
	case nemau.TargetInstance, nemau.TargetLoadBalancer:
	default:
		resp.Diagnostics.AddAttributeError(tfpathRoot("target_type"), "Unknown target",
			"target_type must be \""+nemau.TargetInstance+"\" or \""+nemau.TargetLoadBalancer+"\".")
	}
}

func (r *floatingIPAssociationResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan floatingIPAssociationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	runAndWait(ctx, r.client, &resp.Diagnostics, "Floating IP association", func() (*nemau.TaskRef, error) {
		return r.client.Network.AssociateFloatingIP(ctx, plan.FloatingIPID.ValueString(),
			plan.TargetType.ValueString(), plan.TargetID.ValueString())
	})
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = plan.FloatingIPID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *floatingIPAssociationResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state floatingIPAssociationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	fip, err := r.client.Network.GetFloatingIP(ctx, state.FloatingIPID.ValueString())
	if handleReadError(err, "floating IP association", resp) {
		return
	}
	// The association is gone when the address points at nothing. Removing the
	// resource from state makes the next plan reattach it.
	if fip.TargetID == "" {
		resp.State.RemoveResource(ctx)
		return
	}
	state.TargetType = types.StringValue(fip.TargetType)
	state.TargetID = types.StringValue(fip.TargetID)
	state.ID = state.FloatingIPID
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *floatingIPAssociationResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan floatingIPAssociationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *floatingIPAssociationResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state floatingIPAssociationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Disassociate without releasing: the allocation belongs to nemau_floating_ip.
	deleteAndWait(ctx, r.client, &resp.Diagnostics, "floating IP association", func() (*nemau.TaskRef, error) {
		return r.client.Network.DisassociateFloatingIP(ctx, state.FloatingIPID.ValueString())
	})
}

// ---------------------------------------------------------------------------
// Security group and rule
// ---------------------------------------------------------------------------

var (
	_ resource.Resource                = (*securityGroupResource)(nil)
	_ resource.ResourceWithConfigure   = (*securityGroupResource)(nil)
	_ resource.ResourceWithImportState = (*securityGroupResource)(nil)
	_ resource.ResourceWithModifyPlan  = (*securityGroupResource)(nil)
)

func NewSecurityGroupResource() resource.Resource { return &securityGroupResource{} }

type securityGroupResource struct{ client *nemau.Client }

type securityGroupModel struct {
	ID              types.String `tfsdk:"id"`
	Name            types.String `tfsdk:"name"`
	Description     types.String `tfsdk:"description"`
	EgressFiltering types.Bool   `tfsdk:"egress_filtering"`
}

func (r *securityGroupResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_security_group"
}

func (r *securityGroupResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A security group. Rules are separate resources " +
			"(`nemau_security_group_rule`), as they are in the API. A group created now " +
			"starts with two egress rules that allow all outbound traffic (`0.0.0.0/0` and " +
			"`::/0`); they are not managed by this resource.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Security group id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Security group name. Changes in place.",
			},
			"description": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Free-form description. Changes in place.",
			},
			"egress_filtering": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Whether the group's egress rules are enforced. Always `true` for a " +
					"group created now. `false` for a group created before egress filtering existed: " +
					"all outbound traffic is allowed and its egress rules have no effect. Setting it " +
					"from `false` to `true` converts the group in place: two allow-all egress rules " +
					"are added, so traffic is unchanged until they are removed or narrowed. The " +
					"conversion is one-way: `true` to `false` is refused at plan time.",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *securityGroupResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromResourceProvider(req, resp)
}

// egressFilteringRefusal says why a planned egress_filtering is refused, or
// returns empty strings when it is allowed. configured is the value written in
// the configuration (null when omitted), prior the value in state (null on a
// create).
//
// The refusal happens at plan time: the API cannot undo the conversion, and an
// apply that fails halfway may already have changed other resources.
func egressFilteringRefusal(configured, prior types.Bool) (summary, detail string) {
	if configured.IsNull() || configured.IsUnknown() || configured.ValueBool() {
		return "", ""
	}
	if prior.IsNull() || prior.IsUnknown() {
		return "A new security group always filters egress",
			"Every security group created now enforces its egress rules, and starts with two " +
				"rules that allow all outbound traffic (0.0.0.0/0 and ::/0). Remove " +
				"`egress_filtering = false`. To allow all outbound traffic, keep those two rules."
	}
	if prior.ValueBool() {
		return "Egress filtering cannot be turned off",
			"The conversion to egress filtering is one-way: turning it off would disable every " +
				"egress rule of this group without warning. Remove `egress_filtering = false` " +
				"or set it to true. To allow all outbound traffic, keep the two allow-all egress rules " +
				"(0.0.0.0/0 and ::/0)."
	}
	return "", ""
}

// ModifyPlan refuses turning egress filtering off.
func (r *securityGroupResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // destroy
	}
	var configured types.Bool
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, tfpathRoot("egress_filtering"), &configured)...)
	prior := types.BoolNull()
	if !req.State.Raw.IsNull() {
		resp.Diagnostics.Append(req.State.GetAttribute(ctx, tfpathRoot("egress_filtering"), &prior)...)
	}
	if resp.Diagnostics.HasError() {
		return
	}
	if summary, detail := egressFilteringRefusal(configured, prior); summary != "" {
		resp.Diagnostics.AddAttributeError(tfpathRoot("egress_filtering"), summary, detail)
	}
}

func (r *securityGroupResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan securityGroupModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := createAndWait(ctx, r.client, &resp.Diagnostics, "security group", func() (*nemau.TaskRef, error) {
		return r.client.Network.CreateSecurityGroup(ctx, plan.Name.ValueString(), plan.Description.ValueString())
	}, "security_group_id")
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(id)
	// Read the value back so that the state records what the API enforces.
	// Every group created now filters egress, so true is kept when the read
	// fails; the next refresh corrects it if needed.
	filtering := true
	if group, err := r.client.Network.GetSecurityGroup(ctx, id); err == nil {
		filtering = group.EgressFiltering
	}
	// A group created without egress filtering, in a region that does not
	// enable it by default, is converted when the configuration asks for it.
	if !filtering && plan.EgressFiltering.ValueBool() {
		runAndWait(ctx, r.client, &resp.Diagnostics, "egress filtering conversion", func() (*nemau.TaskRef, error) {
			return r.client.Network.EnableSecurityGroupEgressFiltering(ctx, id)
		})
		filtering = !resp.Diagnostics.HasError()
	}
	plan.EgressFiltering = types.BoolValue(filtering)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *securityGroupResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state securityGroupModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	group, err := r.client.Network.GetSecurityGroup(ctx, state.ID.ValueString())
	if handleReadError(err, "security group", resp) {
		return
	}
	state.Name = types.StringValue(group.Name)
	state.Description = str(group.Description)
	state.EgressFiltering = types.BoolValue(group.EgressFiltering)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *securityGroupResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state securityGroupModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	update := nemau.Update{}
	// Only what actually changed is sent: a PATCH carrying every attribute
	// would overwrite a field another writer touched between the plan and
	// the apply.
	if !plan.Name.Equal(state.Name) {
		update.Name = nemau.Set(plan.Name.ValueString())
	}
	if !plan.Description.Equal(state.Description) {
		update.Description = nemau.Set(plan.Description.ValueString())
	}

	id := state.ID.ValueString()
	// A change to egress_filtering alone sends no PATCH, since the update
	// would be empty.
	if update.Name != nil || update.Description != nil {
		runAndWait(ctx, r.client, &resp.Diagnostics, "security group update", func() (*nemau.TaskRef, error) {
			return r.client.Network.UpdateSecurityGroup(ctx, id, update)
		})
		if resp.Diagnostics.HasError() {
			return
		}
	}
	// false to true is the conversion; ModifyPlan has already refused the way
	// back. It adds two allow-all egress rules, so traffic is unchanged.
	if plan.EgressFiltering.ValueBool() && !state.EgressFiltering.ValueBool() {
		runAndWait(ctx, r.client, &resp.Diagnostics, "egress filtering conversion", func() (*nemau.TaskRef, error) {
			return r.client.Network.EnableSecurityGroupEgressFiltering(ctx, id)
		})
		if resp.Diagnostics.HasError() {
			return
		}
	}
	plan.ID = state.ID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *securityGroupResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state securityGroupModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleteAndWait(ctx, r.client, &resp.Diagnostics, "security group", func() (*nemau.TaskRef, error) {
		return r.client.Network.DeleteSecurityGroup(ctx, state.ID.ValueString())
	})
}

func (r *securityGroupResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importByID(ctx, req, resp)
}

var (
	_ resource.Resource                = (*securityGroupRuleResource)(nil)
	_ resource.ResourceWithConfigure   = (*securityGroupRuleResource)(nil)
	_ resource.ResourceWithImportState = (*securityGroupRuleResource)(nil)
)

func NewSecurityGroupRuleResource() resource.Resource { return &securityGroupRuleResource{} }

type securityGroupRuleResource struct{ client *nemau.Client }

type securityGroupRuleModel struct {
	ID              types.String `tfsdk:"id"`
	SecurityGroupID types.String `tfsdk:"security_group_id"`
	Direction       types.String `tfsdk:"direction"`
	Protocol        types.String `tfsdk:"protocol"`
	PortRangeMin    types.Int64  `tfsdk:"port_range_min"`
	PortRangeMax    types.Int64  `tfsdk:"port_range_max"`
	RemoteIPPrefix  types.String `tfsdk:"remote_ip_prefix"`
}

func (r *securityGroupRuleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_security_group_rule"
}

func (r *securityGroupRuleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A security-group rule. Immutable: any change recreates it.\n\n" +
			"Imports as `<security_group_id>:<rule_id>`, since a rule is only readable through its " +
			"group; `nemau network security-group rules <group-id>` lists the rule ids. Importing " +
			"is how the two default egress rules of a group (`0.0.0.0/0` and `::/0`) come under " +
			"Terraform management, to be kept or deleted.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Rule id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"security_group_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Id of the security group the rule belongs to.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"direction": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "`ingress` or `egress`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"protocol": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "`tcp`, `udp`, `icmp`. When omitted, the rule covers every protocol.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"port_range_min": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "First port of the range, for `tcp` and `udp`. Set it equal to `port_range_max` for a single port.",
				PlanModifiers:       []planmodifier.Int64{int64RequiresReplace()},
			},
			"port_range_max": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Last port of the range, for `tcp` and `udp`.",
				PlanModifiers:       []planmodifier.Int64{int64RequiresReplace()},
			},
			"remote_ip_prefix": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Remote prefix, for example `0.0.0.0/0`: the source of ingress traffic, the destination of egress traffic.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
		},
	}
}

func (r *securityGroupRuleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromResourceProvider(req, resp)
}

func (r *securityGroupRuleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan securityGroupRuleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := createAndWait(ctx, r.client, &resp.Diagnostics, "rule", func() (*nemau.TaskRef, error) {
		return r.client.Network.CreateSecurityGroupRule(ctx, plan.SecurityGroupID.ValueString(),
			nemau.SecurityGroupRuleRequest{
				Direction:      plan.Direction.ValueString(),
				Protocol:       plan.Protocol.ValueString(),
				PortRangeMin:   int(plan.PortRangeMin.ValueInt64()),
				PortRangeMax:   int(plan.PortRangeMax.ValueInt64()),
				RemoteIPPrefix: plan.RemoteIPPrefix.ValueString(),
			})
	}, "rule_id")
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(id)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *securityGroupRuleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state securityGroupRuleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Rules are only readable through their group.
	group, err := r.client.Network.GetSecurityGroup(ctx, state.SecurityGroupID.ValueString())
	if handleReadError(err, "rule", resp) {
		return
	}
	for _, rule := range group.Rules {
		if rule.ID == state.ID.ValueString() {
			state.Direction = types.StringValue(rule.Direction)
			state.Protocol = str(rule.Protocol)
			state.RemoteIPPrefix = str(rule.RemoteIPPrefix)
			if rule.PortRangeMin != 0 || rule.PortRangeMax != 0 {
				state.PortRangeMin = types.Int64Value(int64(rule.PortRangeMin))
				state.PortRangeMax = types.Int64Value(int64(rule.PortRangeMax))
			}
			resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
			return
		}
	}
	resp.State.RemoveResource(ctx)
}

func (r *securityGroupRuleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan securityGroupRuleModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// ImportState takes `<security_group_id>:<rule_id>`. The group is part of the
// id because the API serves a rule only inside its group's listing, so Read
// needs both.
func (r *securityGroupRuleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	groupID, ruleID, ok := splitRuleImportID(req.ID)
	if !ok {
		resp.Diagnostics.AddError("Invalid import id",
			fmt.Sprintf("Expected <security_group_id>:<rule_id>, got %q. "+
				"`nemau network security-group rules <group-id>` lists the rule ids of a group.", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, tfpathRoot("id"), ruleID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, tfpathRoot("security_group_id"), groupID)...)
}

// splitRuleImportID splits `<security_group_id>:<rule_id>`, both non-empty.
func splitRuleImportID(id string) (groupID, ruleID string, ok bool) {
	groupID, ruleID, found := strings.Cut(strings.TrimSpace(id), ":")
	if !found || groupID == "" || ruleID == "" || strings.Contains(ruleID, ":") {
		return "", "", false
	}
	return groupID, ruleID, true
}

func (r *securityGroupRuleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state securityGroupRuleModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleteAndWait(ctx, r.client, &resp.Diagnostics, "rule", func() (*nemau.TaskRef, error) {
		return r.client.Network.DeleteSecurityGroupRule(ctx,
			state.SecurityGroupID.ValueString(), state.ID.ValueString())
	})
}
