package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

// ---------------------------------------------------------------------------
// DNS zone
// ---------------------------------------------------------------------------

var (
	_ resource.Resource                = (*dnsZoneResource)(nil)
	_ resource.ResourceWithConfigure   = (*dnsZoneResource)(nil)
	_ resource.ResourceWithImportState = (*dnsZoneResource)(nil)
)

func NewDNSZoneResource() resource.Resource { return &dnsZoneResource{} }

type dnsZoneResource struct{ client *nemau.Client }

type dnsZoneModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Email       types.String `tfsdk:"email"`
	Description types.String `tfsdk:"description"`
	TTL         types.Int64  `tfsdk:"ttl"`
	Status      types.String `tfsdk:"status"`

	VerificationRecordName  types.String `tfsdk:"verification_record_name"`
	VerificationRecordValue types.String `tfsdk:"verification_record_value"`
}

func (r *dnsZoneResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dns_zone"
}

func (r *dnsZoneResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An authoritative DNS zone.\n\n" +
			"`description`, `email` and `ttl` change in place. Changing `name` replaces the zone " +
			"and deletes every record in it, so read the plan before applying such a change.\n\n" +
			"A new zone is not served until you prove that you control its domain. It is created " +
			"with status `PENDING_VERIFICATION`: publish a TXT record named " +
			"`verification_record_name` holding `verification_record_value` at the domain's " +
			"current DNS host, then declare a " +
			"`nemau_dns_zone_verification`, which waits for the platform to find it. Record sets " +
			"are refused on a pending zone, so take their `zone_id` from the verification resource.\n\n" +
			"The API stops returning the record once the zone is active. The provider keeps the " +
			"values it last read instead of clearing them, so a TXT record declared from them with " +
			"another DNS provider does not show a diff after the verification.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The zone's id, a UUID. Import takes this id, not the zone's name.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Zone name, fully qualified, for example `example.com.`. Changing it replaces the zone and deletes its records.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"email": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Contact address published in the zone's SOA record, for example `hostmaster@example.com`. Changes in place.",
			},
			"description": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Free-form description, one line. Changes in place.",
			},
			"ttl": schema.Int64Attribute{
				Optional: true, Computed: true,
				Default:             int64default.StaticInt64(300),
				MarkdownDescription: "Default TTL of the zone, in seconds, `300` when not set. Changes in place.",
			},
			"status": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "`PENDING_VERIFICATION` until the domain's ownership is proven, " +
					"then `ACTIVE`. Refreshed on every read; an in-place update keeps the last value read.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"verification_record_name": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Fully qualified name of the TXT record that proves ownership, " +
					"such as `_nemau-challenge.example.com.`. Kept after the verification; null " +
					"for a zone imported once already active.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"verification_record_value": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The exact text the TXT record must hold " +
					"(`nemau-verification=…`). Kept after the verification, like " +
					"`verification_record_name`.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *dnsZoneResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromResourceProvider(req, resp)
}

func (r *dnsZoneResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan dnsZoneModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := createAndWait(ctx, r.client, &resp.Diagnostics, "DNS zone", func() (*nemau.TaskRef, error) {
		return r.client.DNS.CreateZone(ctx, nemau.CreateZoneRequest{
			Name:        plan.Name.ValueString(),
			Email:       plan.Email.ValueString(),
			Description: plan.Description.ValueString(),
			TTL:         int(plan.TTL.ValueInt64()),
		})
	}, "zone_id")
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(id)
	plan.Status = types.StringValue("unknown")
	plan.VerificationRecordName = types.StringNull()
	plan.VerificationRecordValue = types.StringNull()
	if zone, err := r.client.DNS.GetZone(ctx, id); err == nil {
		plan.Status = str(zone.Status)
		applyZoneVerification(&plan, zone)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// applyZoneVerification copies the ownership record the API reports. Once the
// zone is active the API stops returning it, and the model keeps the values it
// already holds.
//
// These computed attributes are meant to feed a TXT record declared with the
// domain's current DNS provider. If they became null after the verification,
// the next plan for that record would try to clear its value, on a resource
// this provider does not manage.
func applyZoneVerification(model *dnsZoneModel, zone *nemau.DNSZone) {
	if v := zone.Verification; v != nil {
		model.VerificationRecordName = types.StringValue(v.Name)
		model.VerificationRecordValue = types.StringValue(v.Value)
	}
}

func (r *dnsZoneResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state dnsZoneModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	zone, err := r.client.DNS.GetZone(ctx, state.ID.ValueString())
	if handleReadError(err, "DNS zone", resp) {
		return
	}
	state.Name = types.StringValue(zone.Name)
	state.Email = str(zone.Email)
	state.Description = str(zone.Description)
	state.Status = str(zone.Status)
	applyZoneVerification(&state, zone)
	if zone.TTL != 0 {
		state.TTL = types.Int64Value(int64(zone.TTL))
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *dnsZoneResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state dnsZoneModel
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
	if !plan.Email.Equal(state.Email) {
		update.Email = nemau.Set(plan.Email.ValueString())
	}
	if !plan.TTL.Equal(state.TTL) {
		update.TTL = nemau.Set(int(plan.TTL.ValueInt64()))
	}

	id := state.ID.ValueString()
	runAndWait(ctx, r.client, &resp.Diagnostics, "DNS zone update", func() (*nemau.TaskRef, error) {
		return r.client.DNS.UpdateZone(ctx, id, update)
	})
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = state.ID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *dnsZoneResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state dnsZoneModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleteAndWait(ctx, r.client, &resp.Diagnostics, "DNS zone", func() (*nemau.TaskRef, error) {
		return r.client.DNS.DeleteZone(ctx, state.ID.ValueString())
	})
}

func (r *dnsZoneResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importByID(ctx, req, resp)
}

// ---------------------------------------------------------------------------
// Record set
// ---------------------------------------------------------------------------

var (
	_ resource.Resource              = (*dnsRecordSetResource)(nil)
	_ resource.ResourceWithConfigure = (*dnsRecordSetResource)(nil)
)

func NewDNSRecordSetResource() resource.Resource { return &dnsRecordSetResource{} }

type dnsRecordSetResource struct{ client *nemau.Client }

type dnsRecordSetModel struct {
	ID          types.String `tfsdk:"id"`
	ZoneID      types.String `tfsdk:"zone_id"`
	Name        types.String `tfsdk:"name"`
	Type        types.String `tfsdk:"type"`
	Records     types.List   `tfsdk:"records"`
	TTL         types.Int64  `tfsdk:"ttl"`
	Description types.String `tfsdk:"description"`
}

func (r *dnsRecordSetResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dns_record_set"
}

func (r *dnsRecordSetResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A DNS record set.\n\n" +
			"`records`, `ttl` and `description` change in place. Changing `name` or `type` " +
			"forces a replacement.\n\n" +
			"The zone must be verified: the API refuses record sets on a zone still " +
			"`PENDING_VERIFICATION`. Set `zone_id` from `nemau_dns_zone_verification.<name>.zone_id` " +
			"rather than from the zone, so that Terraform waits for the proof before writing records.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The set's id, shaped `canonical.name./TYPE`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"zone_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The zone's id. Take it from `nemau_dns_zone_verification.<name>.zone_id` " +
					"so the record set depends on the verified zone.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Fully qualified record name inside the zone, for example `www.example.com.`. Changing it replaces the record set.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"type": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Record type, such as `A`, `AAAA`, `CNAME`, `MX` or `TXT`. The platform manages " +
					"`SOA` and the apex `NS` set. Changing it replaces the record set.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"records": schema.ListAttribute{
				Required:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "The record values, 1 to 100, one line each. Change in place.",
			},
			"ttl": schema.Int64Attribute{
				Optional: true, Computed: true,
				Default:             int64default.StaticInt64(300),
				MarkdownDescription: "TTL in seconds, `300` when not set. Changes in place.",
			},
			"description": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Free-form description, one line. Changes in place.",
			},
		},
	}
}

func (r *dnsRecordSetResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromResourceProvider(req, resp)
}

func (r *dnsRecordSetResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan dnsRecordSetModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	records := stringsFromList(ctx, plan.Records, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	id := createAndWait(ctx, r.client, &resp.Diagnostics, "record set", func() (*nemau.TaskRef, error) {
		ref, err := r.client.DNS.CreateRecordSet(ctx, plan.ZoneID.ValueString(), nemau.CreateRecordSetRequest{
			Name:        plan.Name.ValueString(),
			Type:        plan.Type.ValueString(),
			Records:     records,
			TTL:         int(plan.TTL.ValueInt64()),
			Description: plan.Description.ValueString(),
		})
		return ref, pendingZoneHint(err)
	}, "recordset_id")
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(id)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *dnsRecordSetResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state dnsRecordSetModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	recordSet, err := r.client.DNS.GetRecordSet(ctx, state.ZoneID.ValueString(), state.ID.ValueString())
	if handleReadError(err, "record set", resp) {
		return
	}
	state.Name = types.StringValue(recordSet.Name)
	state.Type = types.StringValue(recordSet.Type)
	state.Records = strList(recordSet.Records)
	state.Description = str(recordSet.Description)
	if recordSet.TTL != 0 {
		state.TTL = types.Int64Value(int64(recordSet.TTL))
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *dnsRecordSetResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state dnsRecordSetModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	records := stringsFromList(ctx, plan.Records, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = state.ID
	runAndWait(ctx, r.client, &resp.Diagnostics, "Record-set update", func() (*nemau.TaskRef, error) {
		return r.client.DNS.UpdateRecordSet(ctx, plan.ZoneID.ValueString(), state.ID.ValueString(),
			nemau.UpdateRecordSetRequest{
				Records:     records,
				TTL:         int(plan.TTL.ValueInt64()),
				Description: plan.Description.ValueString(),
			})
	})
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *dnsRecordSetResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state dnsRecordSetModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleteAndWait(ctx, r.client, &resp.Diagnostics, "record set", func() (*nemau.TaskRef, error) {
		return r.client.DNS.DeleteRecordSet(ctx, state.ZoneID.ValueString(), state.ID.ValueString())
	})
}

// pendingZoneHint says what to do about a record set refused because its zone
// has not been verified: the usual cause is a zone_id taken from the zone
// rather than from its verification, which lets Terraform write records first.
func pendingZoneHint(err error) error {
	if nemau.HasReason(err, nemau.ReasonDNSZonePendingVerification) {
		return fmt.Errorf("%w\n\nThe zone's ownership has not been verified yet. Declare a "+
			"nemau_dns_zone_verification for it and set this record set's zone_id from "+
			"nemau_dns_zone_verification.<name>.zone_id, so it is created after the proof.", err)
	}
	return err
}
