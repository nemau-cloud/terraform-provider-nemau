package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

// The load balancer, listener, pool, member and monitor are separate resources
// because the API models them that way. With a single resource, a member
// change would recreate the whole load balancer.
//
// Names, descriptions, the balancing algorithm, a member's weight, a
// monitor's timings and a listener's certificate change in place: the
// platform rewrites the appliance configuration from them without replacing
// the appliance. Attributes that identify an object force a replacement: the
// subnet a VIP is allocated in, a load balancer's tier, a listener's protocol
// and port, a member's address.

// ---------------------------------------------------------------------------
// Load balancer
// ---------------------------------------------------------------------------

var (
	_ resource.Resource                = (*loadBalancerResource)(nil)
	_ resource.ResourceWithConfigure   = (*loadBalancerResource)(nil)
	_ resource.ResourceWithImportState = (*loadBalancerResource)(nil)
)

func NewLoadBalancerResource() resource.Resource { return &loadBalancerResource{} }

type loadBalancerResource struct{ client *nemau.Client }

type loadBalancerModel struct {
	ID                 types.String `tfsdk:"id"`
	Name               types.String `tfsdk:"name"`
	Description        types.String `tfsdk:"description"`
	VipSubnetID        types.String `tfsdk:"vip_subnet_id"`
	Provider           types.String `tfsdk:"provider_engine"`
	VipAddress         types.String `tfsdk:"vip_address"`
	ProvisioningStatus types.String `tfsdk:"provisioning_status"`
	OperatingStatus    types.String `tfsdk:"operating_status"`
	Flavor             types.String `tfsdk:"flavor"`
	ApplianceAddresses types.List   `tfsdk:"appliance_addresses"`
}

// defaultLoadBalancerFlavor is the tier the API gives a load balancer created
// without one.
const defaultLoadBalancerFlavor = "lb-small"

// defaultLoadBalancerEngine is the engine the API gives a load balancer
// created without one, which is every load balancer this provider creates. It
// serves all listener protocols.
const defaultLoadBalancerEngine = "auto"

func (r *loadBalancerResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_load_balancer"
}

func (r *loadBalancerResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A load balancer. Its VIP is allocated in `vip_subnet_id`; `flavor` " +
			"picks the tier, and the high-availability tiers run two appliances on two hosts behind that VIP.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Load balancer id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Load balancer name. Changes in place.",
			},
			"description": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Free-form description. Changes in place.",
			},
			"vip_subnet_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Id of the subnet the VIP is allocated in. Changing it replaces the load balancer.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"provider_engine": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The engine the load balancer runs. It is read-only. " +
					"A new load balancer is always `auto`, which accepts `HTTP`, `HTTPS`, `TCP` and `UDP` " +
					"listeners side by side; an older load balancer may report `haproxy` (no `UDP` " +
					"listeners) or `nginx` (`UDP` listeners only). The attribute is named " +
					"`provider_engine` because `provider` is reserved in Terraform.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"vip_address": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The VIP: the address clients connect to, taken from `vip_subnet_id`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"provisioning_status": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Provisioning status as reported by the platform.",
			},
			"operating_status": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Operating status as reported by the platform.",
			},
			"flavor": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "The tier: `lb-small` (one appliance, the default), or `lb-ha`, " +
					"`lb-medium`, `lb-large` (two appliances on two hosts, active and standby behind the " +
					"VIP). The platform does not change a tier in place, so changing it replaces the load " +
					"balancer and allocates a new VIP.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
					stringplanmodifier.RequiresReplace(),
				},
			},
			"appliance_addresses": schema.ListAttribute{
				ElementType: types.StringType,
				Computed:    true,
				MarkdownDescription: "The addresses health checks and proxied connections leave from: " +
					"the VIP on `lb-small`, each appliance's own address on a high-availability tier. " +
					"The members' security groups must admit them.",
				PlanModifiers: []planmodifier.List{listplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *loadBalancerResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromResourceProvider(req, resp)
}

func (r *loadBalancerResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan loadBalancerModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := createAndWait(ctx, r.client, &resp.Diagnostics, "load balancer", func() (*nemau.TaskRef, error) {
		return r.client.LoadBalancer.Create(ctx, nemau.CreateLoadBalancerRequest{
			Name:        plan.Name.ValueString(),
			Description: plan.Description.ValueString(),
			VipSubnetID: plan.VipSubnetID.ValueString(),
			Flavor:      plan.Flavor.ValueString(),
		})
	}, "load_balancer_id")
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(id)
	if lb, err := r.client.LoadBalancer.Get(ctx, id); err == nil {
		applyLoadBalancer(&plan, lb)
	} else {
		plan.VipAddress = types.StringValue("")
		plan.ProvisioningStatus = types.StringValue("unknown")
		plan.OperatingStatus = types.StringValue("unknown")
		if plan.Flavor.IsUnknown() {
			plan.Flavor = types.StringValue(defaultLoadBalancerFlavor)
		}
		plan.Provider = types.StringValue(defaultLoadBalancerEngine)
		plan.ApplianceAddresses = strList(nil)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *loadBalancerResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state loadBalancerModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	lb, err := r.client.LoadBalancer.Get(ctx, state.ID.ValueString())
	if handleReadError(err, "load balancer", resp) {
		return
	}
	applyLoadBalancer(&state, lb)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *loadBalancerResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state loadBalancerModel
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
	runAndWait(ctx, r.client, &resp.Diagnostics, "load balancer update", func() (*nemau.TaskRef, error) {
		return r.client.LoadBalancer.UpdateLoadBalancer(ctx, id, update)
	})
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = state.ID
	// The statuses are computed without a plan modifier, so any change plans
	// them unknown; they are read back rather than written as unknown.
	if lb, err := r.client.LoadBalancer.Get(ctx, id); err == nil {
		applyLoadBalancer(&plan, lb)
	} else {
		plan.ProvisioningStatus, plan.OperatingStatus = state.ProvisioningStatus, state.OperatingStatus
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *loadBalancerResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state loadBalancerModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleteAndWait(ctx, r.client, &resp.Diagnostics, "load balancer", func() (*nemau.TaskRef, error) {
		return r.client.LoadBalancer.Delete(ctx, state.ID.ValueString())
	})
}

func (r *loadBalancerResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importByID(ctx, req, resp)
}

func applyLoadBalancer(model *loadBalancerModel, lb *nemau.LoadBalancer) {
	model.ID = types.StringValue(lb.ID)
	model.Name = types.StringValue(lb.Name)
	model.Description = str(lb.Description)
	model.VipAddress = types.StringValue(lb.VipAddress)
	model.ProvisioningStatus = str(lb.ProvisioningStatus)
	model.OperatingStatus = str(lb.OperatingStatus)
	if lb.VipSubnetID != "" {
		model.VipSubnetID = types.StringValue(lb.VipSubnetID)
	}
	engine := lb.Provider
	if engine == "" {
		engine = defaultLoadBalancerEngine
	}
	model.Provider = types.StringValue(engine)
	flavor := lb.Flavor
	if flavor == "" {
		flavor = defaultLoadBalancerFlavor
	}
	model.Flavor = types.StringValue(flavor)
	model.ApplianceAddresses = strList(loadBalancerSourceAddresses(lb))
}

// loadBalancerSourceAddresses is where a load balancer's traffic to its members
// comes from. A single appliance's own address is the VIP; a high-availability
// pair sends from each appliance's address, never from the VIP.
func loadBalancerSourceAddresses(lb *nemau.LoadBalancer) []string {
	if lb.HighAvailability == nil {
		if lb.VipAddress == "" {
			return nil
		}
		return []string{lb.VipAddress}
	}
	addresses := make([]string, 0, len(lb.HighAvailability.Appliances))
	for _, appliance := range lb.HighAvailability.Appliances {
		if appliance.Address != "" {
			addresses = append(addresses, appliance.Address)
		}
	}
	return addresses
}

// ---------------------------------------------------------------------------
// Listener
// ---------------------------------------------------------------------------

var (
	_ resource.Resource              = (*listenerResource)(nil)
	_ resource.ResourceWithConfigure = (*listenerResource)(nil)
)

func NewListenerResource() resource.Resource { return &listenerResource{} }

type listenerResource struct{ client *nemau.Client }

type listenerModel struct {
	ID                types.String `tfsdk:"id"`
	LoadBalancerID    types.String `tfsdk:"load_balancer_id"`
	Name              types.String `tfsdk:"name"`
	Description       types.String `tfsdk:"description"`
	Protocol          types.String `tfsdk:"protocol"`
	ProtocolPort      types.Int64  `tfsdk:"protocol_port"`
	DefaultPoolID     types.String `tfsdk:"default_pool_id"`
	ConnectionLimit   types.Int64  `tfsdk:"connection_limit"`
	TLSCertificateRef types.String `tfsdk:"tls_certificate_ref"`
	AdminStateUp      types.Bool   `tfsdk:"admin_state_up"`
	OperatingStatus   types.String `tfsdk:"operating_status"`
}

func (r *listenerResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_listener"
}

func (r *listenerResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A listening port on a load balancer.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Listener id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"load_balancer_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Id of the load balancer the listener belongs to.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Listener name. Changes in place.",
			},
			"description": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Free-form description. Changes in place.",
			},
			"protocol": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "`HTTP`, `HTTPS` (passed through), `TERMINATED_HTTPS` (decrypted on the " +
					"load balancer, needs `tls_certificate_ref`), `TCP` or `UDP`.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"protocol_port": schema.Int64Attribute{
				Required:            true,
				MarkdownDescription: "Port the listener accepts connections on.",
				PlanModifiers:       []planmodifier.Int64{int64RequiresReplace()},
			},
			"default_pool_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Id of the pool that receives the traffic.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"connection_limit": schema.Int64Attribute{
				Optional: true,
				MarkdownDescription: "Maximum number of concurrent connections, 0 to 1000000. Not supported " +
					"on `UDP` listeners. Changes in place.",
			},
			// Changes in place: repointing the listener is how a certificate is
			// renewed, and the appliance reloads without dropping connections.
			"tls_certificate_ref": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "ID of the `nemau_load_balancer_certificate` a `TERMINATED_HTTPS` listener " +
					"serves; required for that protocol, refused for the others. Changes in place.",
			},
			"admin_state_up": schema.BoolAttribute{
				Optional: true, Computed: true,
				Default:             booldefault.StaticBool(true),
				MarkdownDescription: "Administrative state, `true` by default; `false` takes the listener out of service. Changing it replaces the listener.",
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
			},
			"operating_status": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Operating status as reported by the platform.",
			},
		},
	}
}

func (r *listenerResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromResourceProvider(req, resp)
}

func (r *listenerResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan listenerModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	adminState := plan.AdminStateUp.ValueBool()
	id := createAndWait(ctx, r.client, &resp.Diagnostics, "listener", func() (*nemau.TaskRef, error) {
		return r.client.LoadBalancer.CreateListener(ctx, plan.LoadBalancerID.ValueString(),
			nemau.CreateListenerRequest{
				Name:              plan.Name.ValueString(),
				Description:       plan.Description.ValueString(),
				Protocol:          plan.Protocol.ValueString(),
				ProtocolPort:      int(plan.ProtocolPort.ValueInt64()),
				DefaultPoolID:     plan.DefaultPoolID.ValueString(),
				ConnectionLimit:   int(plan.ConnectionLimit.ValueInt64()),
				TLSCertificateRef: plan.TLSCertificateRef.ValueString(),
				AdminStateUp:      &adminState,
			})
	}, "listener_id")
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(id)
	plan.OperatingStatus = types.StringValue("unknown")
	if listener, err := r.client.LoadBalancer.GetListener(ctx, plan.LoadBalancerID.ValueString(), id); err == nil {
		plan.OperatingStatus = str(listener.OperatingStatus)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *listenerResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state listenerModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	listener, err := r.client.LoadBalancer.GetListener(ctx,
		state.LoadBalancerID.ValueString(), state.ID.ValueString())
	if handleReadError(err, "listener", resp) {
		return
	}
	state.Name = types.StringValue(listener.Name)
	state.Description = str(listener.Description)
	state.Protocol = types.StringValue(listener.Protocol)
	state.ProtocolPort = types.Int64Value(int64(listener.ProtocolPort))
	state.DefaultPoolID = str(listener.DefaultPoolID)
	state.TLSCertificateRef = str(listener.TLSCertificateRef)
	state.AdminStateUp = types.BoolValue(listener.AdminStateUp)
	state.OperatingStatus = str(listener.OperatingStatus)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *listenerResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state listenerModel
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
	if !plan.ConnectionLimit.Equal(state.ConnectionLimit) {
		update.ConnectionLimit = nemau.Set(int(plan.ConnectionLimit.ValueInt64()))
	}
	if !plan.TLSCertificateRef.Equal(state.TLSCertificateRef) {
		update.TLSCertificateRef = nemau.Set(plan.TLSCertificateRef.ValueString())
	}

	id := state.ID.ValueString()
	runAndWait(ctx, r.client, &resp.Diagnostics, "listener update", func() (*nemau.TaskRef, error) {
		return r.client.LoadBalancer.UpdateListener(ctx, id, update)
	})
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = state.ID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *listenerResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state listenerModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleteAndWait(ctx, r.client, &resp.Diagnostics, "listener", func() (*nemau.TaskRef, error) {
		return r.client.LoadBalancer.DeleteListener(ctx, state.ID.ValueString())
	})
}

// ---------------------------------------------------------------------------
// Pool
// ---------------------------------------------------------------------------

var (
	_ resource.Resource              = (*poolResource)(nil)
	_ resource.ResourceWithConfigure = (*poolResource)(nil)
)

func NewPoolResource() resource.Resource { return &poolResource{} }

type poolResource struct{ client *nemau.Client }

type poolModel struct {
	ID              types.String `tfsdk:"id"`
	LoadBalancerID  types.String `tfsdk:"load_balancer_id"`
	Name            types.String `tfsdk:"name"`
	Description     types.String `tfsdk:"description"`
	Protocol        types.String `tfsdk:"protocol"`
	LBAlgorithm     types.String `tfsdk:"lb_algorithm"`
	ListenerID      types.String `tfsdk:"listener_id"`
	AdminStateUp    types.Bool   `tfsdk:"admin_state_up"`
	OperatingStatus types.String `tfsdk:"operating_status"`
}

func (r *poolResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_pool"
}

func (r *poolResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A group of backends behind a load balancer.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Pool id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"load_balancer_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Id of the load balancer the pool belongs to.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Pool name. Changes in place.",
			},
			"description": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Free-form description. Changes in place.",
			},
			"protocol": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Protocol the load balancer uses to reach the members: `HTTP`, `HTTPS`, " +
					"`TCP` or `UDP`. It must suit the listener: a `TERMINATED_HTTPS` listener forwards to " +
					"an `HTTP` pool, and an `HTTPS` listener to an `HTTPS` or `TCP` pool.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"lb_algorithm": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "`ROUND_ROBIN`, `LEAST_CONNECTIONS` or `SOURCE_IP`. Changes in place.",
			},
			"listener_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Id of a listener to attach the pool to at creation. A listener can " +
					"also point at the pool with `default_pool_id`.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"admin_state_up": schema.BoolAttribute{
				Optional: true, Computed: true,
				Default:             booldefault.StaticBool(true),
				MarkdownDescription: "Administrative state, `true` by default; `false` takes the pool out of service. Changing it replaces the pool.",
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
			},
			"operating_status": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Operating status as reported by the platform.",
			},
		},
	}
}

func (r *poolResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromResourceProvider(req, resp)
}

func (r *poolResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan poolModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	adminState := plan.AdminStateUp.ValueBool()
	id := createAndWait(ctx, r.client, &resp.Diagnostics, "pool", func() (*nemau.TaskRef, error) {
		return r.client.LoadBalancer.CreatePool(ctx, plan.LoadBalancerID.ValueString(),
			nemau.CreatePoolRequest{
				Name:         plan.Name.ValueString(),
				Description:  plan.Description.ValueString(),
				Protocol:     plan.Protocol.ValueString(),
				LBAlgorithm:  plan.LBAlgorithm.ValueString(),
				ListenerID:   plan.ListenerID.ValueString(),
				AdminStateUp: &adminState,
			})
	}, "pool_id")
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(id)
	plan.OperatingStatus = types.StringValue("unknown")
	if pool, err := r.client.LoadBalancer.GetPool(ctx, plan.LoadBalancerID.ValueString(), id); err == nil {
		plan.OperatingStatus = str(pool.OperatingStatus)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *poolResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state poolModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	pool, err := r.client.LoadBalancer.GetPool(ctx, state.LoadBalancerID.ValueString(), state.ID.ValueString())
	if handleReadError(err, "pool", resp) {
		return
	}
	state.Name = types.StringValue(pool.Name)
	state.Description = str(pool.Description)
	state.Protocol = types.StringValue(pool.Protocol)
	state.LBAlgorithm = types.StringValue(pool.LBAlgorithm)
	state.ListenerID = str(pool.ListenerID)
	state.AdminStateUp = types.BoolValue(pool.AdminStateUp)
	state.OperatingStatus = str(pool.OperatingStatus)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *poolResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state poolModel
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
	if !plan.LBAlgorithm.Equal(state.LBAlgorithm) {
		update.LBAlgorithm = nemau.Set(plan.LBAlgorithm.ValueString())
	}

	id := state.ID.ValueString()
	runAndWait(ctx, r.client, &resp.Diagnostics, "pool update", func() (*nemau.TaskRef, error) {
		return r.client.LoadBalancer.UpdatePool(ctx, id, update)
	})
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = state.ID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *poolResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state poolModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleteAndWait(ctx, r.client, &resp.Diagnostics, "pool", func() (*nemau.TaskRef, error) {
		return r.client.LoadBalancer.DeletePool(ctx, state.ID.ValueString())
	})
}

// ---------------------------------------------------------------------------
// Pool member
// ---------------------------------------------------------------------------

var (
	_ resource.Resource              = (*poolMemberResource)(nil)
	_ resource.ResourceWithConfigure = (*poolMemberResource)(nil)
)

func NewPoolMemberResource() resource.Resource { return &poolMemberResource{} }

type poolMemberResource struct{ client *nemau.Client }

type poolMemberModel struct {
	ID              types.String `tfsdk:"id"`
	PoolID          types.String `tfsdk:"pool_id"`
	Name            types.String `tfsdk:"name"`
	Address         types.String `tfsdk:"address"`
	ProtocolPort    types.Int64  `tfsdk:"protocol_port"`
	Weight          types.Int64  `tfsdk:"weight"`
	SubnetID        types.String `tfsdk:"subnet_id"`
	AdminStateUp    types.Bool   `tfsdk:"admin_state_up"`
	OperatingStatus types.String `tfsdk:"operating_status"`
}

func (r *poolMemberResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_pool_member"
}

func (r *poolMemberResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A backend in a pool.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Member id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"pool_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Id of the pool the member belongs to.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Member name. Changes in place.",
			},
			"address": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "IP address of the backend.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"protocol_port": schema.Int64Attribute{
				Required:            true,
				MarkdownDescription: "Port the backend listens on.",
				PlanModifiers:       []planmodifier.Int64{int64RequiresReplace()},
			},
			"weight": schema.Int64Attribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Weight in the balancing, `1` when not set. Changes in place.",
			},
			"subnet_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Id of the subnet the backend address belongs to.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"admin_state_up": schema.BoolAttribute{
				Optional: true, Computed: true,
				Default:             booldefault.StaticBool(true),
				MarkdownDescription: "Administrative state, `true` by default; `false` takes the member out of service. Changing it replaces the member.",
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
			},
			"operating_status": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Operating status as reported by the platform.",
			},
		},
	}
}

func (r *poolMemberResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromResourceProvider(req, resp)
}

func (r *poolMemberResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan poolMemberModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	adminState := plan.AdminStateUp.ValueBool()
	id := createAndWait(ctx, r.client, &resp.Diagnostics, "pool member", func() (*nemau.TaskRef, error) {
		return r.client.LoadBalancer.AddPoolMember(ctx, plan.PoolID.ValueString(),
			nemau.AddPoolMemberRequest{
				Name:         plan.Name.ValueString(),
				Address:      plan.Address.ValueString(),
				ProtocolPort: int(plan.ProtocolPort.ValueInt64()),
				Weight:       int(plan.Weight.ValueInt64()),
				SubnetID:     plan.SubnetID.ValueString(),
				AdminStateUp: &adminState,
			})
	}, "member_id")
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(id)
	plan.OperatingStatus = types.StringValue("unknown")
	if member, err := r.client.LoadBalancer.GetPoolMember(ctx, plan.PoolID.ValueString(), id); err == nil {
		plan.Weight = types.Int64Value(int64(member.Weight))
		plan.OperatingStatus = str(member.OperatingStatus)
	} else if plan.Weight.IsUnknown() || plan.Weight.IsNull() {
		// The server default is 1. Store it rather than leaving the value unknown.
		plan.Weight = types.Int64Value(1)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *poolMemberResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state poolMemberModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	member, err := r.client.LoadBalancer.GetPoolMember(ctx, state.PoolID.ValueString(), state.ID.ValueString())
	if handleReadError(err, "pool member", resp) {
		return
	}
	state.Name = types.StringValue(member.Name)
	state.Address = types.StringValue(member.Address)
	state.ProtocolPort = types.Int64Value(int64(member.ProtocolPort))
	state.Weight = types.Int64Value(int64(member.Weight))
	state.SubnetID = str(member.SubnetID)
	state.AdminStateUp = types.BoolValue(member.AdminStateUp)
	state.OperatingStatus = str(member.OperatingStatus)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *poolMemberResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state poolMemberModel
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
	if !plan.Weight.Equal(state.Weight) {
		update.Weight = nemau.Set(int(plan.Weight.ValueInt64()))
	}

	id := state.ID.ValueString()
	runAndWait(ctx, r.client, &resp.Diagnostics, "pool member update", func() (*nemau.TaskRef, error) {
		return r.client.LoadBalancer.UpdatePoolMember(ctx, state.PoolID.ValueString(), id, update)
	})
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = state.ID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *poolMemberResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state poolMemberModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleteAndWait(ctx, r.client, &resp.Diagnostics, "pool member", func() (*nemau.TaskRef, error) {
		return r.client.LoadBalancer.RemovePoolMember(ctx, state.PoolID.ValueString(), state.ID.ValueString())
	})
}

// ---------------------------------------------------------------------------
// Health monitor
// ---------------------------------------------------------------------------

var (
	_ resource.Resource              = (*healthMonitorResource)(nil)
	_ resource.ResourceWithConfigure = (*healthMonitorResource)(nil)
)

func NewHealthMonitorResource() resource.Resource { return &healthMonitorResource{} }

type healthMonitorResource struct{ client *nemau.Client }

type healthMonitorModel struct {
	ID            types.String `tfsdk:"id"`
	PoolID        types.String `tfsdk:"pool_id"`
	Type          types.String `tfsdk:"type"`
	Delay         types.Int64  `tfsdk:"delay"`
	Timeout       types.Int64  `tfsdk:"timeout"`
	MaxRetries    types.Int64  `tfsdk:"max_retries"`
	HTTPMethod    types.String `tfsdk:"http_method"`
	URLPath       types.String `tfsdk:"url_path"`
	ExpectedCodes types.String `tfsdk:"expected_codes"`
	AdminStateUp  types.Bool   `tfsdk:"admin_state_up"`
}

func (r *healthMonitorResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_health_monitor"
}

func (r *healthMonitorResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A health probe on a pool.\n\n" +
			"UDP pools accept only `UDP_CONNECT`. The engine detects failures on UDP passively, " +
			"so the API refuses probe types it would not run.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Health monitor id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"pool_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Id of the pool whose members are probed. A pool has one monitor.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"type": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "`HTTP`, `HTTPS`, `TCP` or `UDP_CONNECT`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"delay": schema.Int64Attribute{
				Required:            true,
				MarkdownDescription: "Interval between two probes, in seconds, 1 to 3600. Changes in place.",
			},
			"timeout": schema.Int64Attribute{
				Required:            true,
				MarkdownDescription: "Timeout for one probe, in seconds, shorter than `delay`. Changes in place.",
			},
			"max_retries": schema.Int64Attribute{
				Required:            true,
				MarkdownDescription: "Consecutive failures before the backend is taken out, 1 to 10. Changes in place.",
			},
			"http_method": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "`GET` (the default), `HEAD` or `OPTIONS`. Used by `HTTP` and `HTTPS` probes.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"url_path": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Absolute path the probe requests, for example `/health`. Used by `HTTP` and `HTTPS` probes.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"expected_codes": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "HTTP codes that count as healthy: codes or ranges separated by commas, " +
					"such as `200` or `200-299`. `200-399` when not set. Changes in place.",
			},
			"admin_state_up": schema.BoolAttribute{
				Optional: true, Computed: true,
				Default:             booldefault.StaticBool(true),
				MarkdownDescription: "Administrative state, `true` by default; `false` takes the health monitor out of service. Changing it replaces the health monitor.",
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
			},
		},
	}
}

func (r *healthMonitorResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromResourceProvider(req, resp)
}

func (r *healthMonitorResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan healthMonitorModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	adminState := plan.AdminStateUp.ValueBool()
	id := createAndWait(ctx, r.client, &resp.Diagnostics, "health monitor", func() (*nemau.TaskRef, error) {
		return r.client.LoadBalancer.CreateHealthMonitor(ctx, plan.PoolID.ValueString(),
			nemau.CreateHealthMonitorRequest{
				Type:          plan.Type.ValueString(),
				Delay:         int(plan.Delay.ValueInt64()),
				Timeout:       int(plan.Timeout.ValueInt64()),
				MaxRetries:    int(plan.MaxRetries.ValueInt64()),
				HTTPMethod:    plan.HTTPMethod.ValueString(),
				URLPath:       plan.URLPath.ValueString(),
				ExpectedCodes: plan.ExpectedCodes.ValueString(),
				AdminStateUp:  &adminState,
			})
	}, "health_monitor_id")
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(id)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *healthMonitorResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state healthMonitorModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	monitor, err := r.client.LoadBalancer.GetHealthMonitor(ctx,
		state.PoolID.ValueString(), state.ID.ValueString())
	if handleReadError(err, "health monitor", resp) {
		return
	}
	state.Type = types.StringValue(monitor.Type)
	state.Delay = types.Int64Value(int64(monitor.Delay))
	state.Timeout = types.Int64Value(int64(monitor.Timeout))
	state.MaxRetries = types.Int64Value(int64(monitor.MaxRetries))
	state.HTTPMethod = str(monitor.HTTPMethod)
	state.URLPath = str(monitor.URLPath)
	state.ExpectedCodes = str(monitor.ExpectedCodes)
	state.AdminStateUp = types.BoolValue(monitor.AdminStateUp)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *healthMonitorResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state healthMonitorModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	update := nemau.Update{}
	// Only what actually changed is sent: a PATCH carrying every attribute
	// would overwrite a field another writer touched between the plan and
	// the apply.
	if !plan.Delay.Equal(state.Delay) {
		update.Delay = nemau.Set(int(plan.Delay.ValueInt64()))
	}
	if !plan.Timeout.Equal(state.Timeout) {
		update.Timeout = nemau.Set(int(plan.Timeout.ValueInt64()))
	}
	if !plan.MaxRetries.Equal(state.MaxRetries) {
		update.MaxRetries = nemau.Set(int(plan.MaxRetries.ValueInt64()))
	}
	if !plan.ExpectedCodes.Equal(state.ExpectedCodes) {
		update.ExpectedCodes = nemau.Set(plan.ExpectedCodes.ValueString())
	}

	id := state.ID.ValueString()
	runAndWait(ctx, r.client, &resp.Diagnostics, "health monitor update", func() (*nemau.TaskRef, error) {
		return r.client.LoadBalancer.UpdateHealthMonitor(ctx, state.PoolID.ValueString(), id, update)
	})
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = state.ID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *healthMonitorResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state healthMonitorModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleteAndWait(ctx, r.client, &resp.Diagnostics, "health monitor", func() (*nemau.TaskRef, error) {
		return r.client.LoadBalancer.DeleteHealthMonitor(ctx,
			state.PoolID.ValueString(), state.ID.ValueString())
	})
}
