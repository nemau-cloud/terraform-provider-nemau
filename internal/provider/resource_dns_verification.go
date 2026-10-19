package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

// ---------------------------------------------------------------------------
// DNS zone verification
// ---------------------------------------------------------------------------

// This resource works like aws_acm_certificate_validation: the zone is created
// pending, something else publishes the TXT record, and this resource is where
// Terraform waits for the platform to find it. It owns nothing on the platform,
// so Delete does nothing.

var (
	_ resource.Resource                   = (*dnsZoneVerificationResource)(nil)
	_ resource.ResourceWithConfigure      = (*dnsZoneVerificationResource)(nil)
	_ resource.ResourceWithImportState    = (*dnsZoneVerificationResource)(nil)
	_ resource.ResourceWithValidateConfig = (*dnsZoneVerificationResource)(nil)
)

// defaultVerificationTimeout covers a TXT record created in the same apply at a
// DNS host whose own propagation takes a few minutes, with room to spare.
const defaultVerificationTimeout = "45m"

// verificationRetryInterval is the pause between two checks. A variable so
// tests do not wait thirty seconds per failed attempt.
var verificationRetryInterval = 30 * time.Second

func NewDNSZoneVerificationResource() resource.Resource { return &dnsZoneVerificationResource{} }

type dnsZoneVerificationResource struct{ client *nemau.Client }

type dnsZoneVerificationModel struct {
	ID      types.String `tfsdk:"id"`
	ZoneID  types.String `tfsdk:"zone_id"`
	Timeout types.String `tfsdk:"timeout"`
}

func (r *dnsZoneVerificationResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dns_zone_verification"
}

func (r *dnsZoneVerificationResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Waits for the platform to verify the ownership of a DNS zone's domain.\n\n" +
			"A `nemau_dns_zone` is created with status `PENDING_VERIFICATION`, and nothing is " +
			"served for it until " +
			"a TXT record named `verification_record_name` and holding `verification_record_value` " +
			"is visible at the domain's current DNS host. Creating this resource asks the platform to " +
			"check that record, and asks again every 30 seconds until it passes or `timeout` runs out: " +
			"a record created in the same apply takes a while to propagate.\n\n" +
			"This resource creates nothing on the platform. Destroying it only removes it from the " +
			"state; the zone stays verified. If the zone is found pending again on a later read, the " +
			"resource leaves the state and the next apply verifies it again.\n\n" +
			"Record sets are refused on a pending zone: take their `zone_id` from this resource's " +
			"`zone_id` so that Terraform creates them after the verification.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Same as `zone_id`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"zone_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The id (a UUID) of the `nemau_dns_zone` to verify: `nemau_dns_zone.<name>.id`, not the zone's name.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"timeout": schema.StringAttribute{
				Optional: true, Computed: true,
				Default: stringdefault.StaticString(defaultVerificationTimeout),
				MarkdownDescription: "How long to keep checking before failing the apply, as a " +
					"duration such as `45m` or `2h`. Default `" + defaultVerificationTimeout + "`.",
			},
		},
	}
}

func (r *dnsZoneVerificationResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromResourceProvider(req, resp)
}

func (r *dnsZoneVerificationResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config dnsZoneVerificationModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() || config.Timeout.IsNull() || config.Timeout.IsUnknown() {
		return
	}
	if _, err := parseVerificationTimeout(config.Timeout.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(tfpathRoot("timeout"), "Invalid timeout", err.Error())
	}
}

func parseVerificationTimeout(raw string) (time.Duration, error) {
	timeout, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration such as 45m or 2h: %v", raw, err)
	}
	if timeout <= 0 {
		return 0, fmt.Errorf("%q: the timeout must be positive", raw)
	}
	return timeout, nil
}

func (r *dnsZoneVerificationResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan dnsZoneVerificationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	zoneID := plan.ZoneID.ValueString()
	timeout, err := parseVerificationTimeout(plan.Timeout.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(tfpathRoot("timeout"), "Invalid timeout", err.Error())
		return
	}

	verifyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := verifyZoneUntilActive(verifyCtx, r.client, zoneID, verificationRetryInterval); err != nil {
		var expired *verificationExpiredError
		if errors.As(err, &expired) {
			resp.Diagnostics.AddError("DNS zone not verified within "+timeout.String(),
				verificationExpiredDetail(ctx, r.client, zoneID, expired))
			return
		}
		resp.Diagnostics.AddError("Verifying DNS zone "+zoneID+" failed", err.Error())
		return
	}

	plan.ID = types.StringValue(zoneID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read keeps the resource only while the zone is active. A zone found pending
// again, or deleted, leaves the state, so the next plan verifies it again, as
// with aws_acm_certificate_validation.
func (r *dnsZoneVerificationResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state dnsZoneVerificationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	zoneID := state.ZoneID.ValueString()
	if zoneID == "" {
		// After an import, the id is the zone id.
		zoneID = state.ID.ValueString()
		state.ZoneID = types.StringValue(zoneID)
	}
	zone, err := r.client.DNS.GetZone(ctx, zoneID)
	if handleReadError(err, "DNS zone", resp) {
		return
	}
	if zone.Status != nemau.DNSZoneStatusActive {
		tflog.Warn(ctx, "DNS zone is not active; its verification leaves the state", map[string]any{
			"zone_id": zoneID, "status": zone.Status,
		})
		resp.State.RemoveResource(ctx)
		return
	}
	state.ID = types.StringValue(zoneID)
	if state.Timeout.IsNull() {
		state.Timeout = types.StringValue(defaultVerificationTimeout)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update only ever sees a new timeout: zone_id forces a replacement. Nothing
// to send.
func (r *dnsZoneVerificationResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state dnsZoneVerificationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = state.ID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete does nothing: a proof of ownership cannot be taken back, and the zone
// stays active. Only destroying the zone stops serving it.
func (r *dnsZoneVerificationResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
}

func (r *dnsZoneVerificationResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importByID(ctx, req, resp)
}

// verificationExpiredError is the deadline running out while the record was
// still not found. last is the most recent failed check, nil when none
// completed.
type verificationExpiredError struct{ last error }

func (e *verificationExpiredError) Error() string {
	if e.last == nil {
		return "the zone was not verified before the timeout"
	}
	return "the zone was not verified before the timeout; last check: " + e.last.Error()
}

// verifyZoneUntilActive asks for the ownership check until it passes, the zone
// turns out to be verified already, or ctx ends.
//
// Only a failed check is retried, because it means the record is not visible
// yet. A refused request (unknown zone, no permission) is returned at once,
// since asking again would get the same answer.
func verifyZoneUntilActive(ctx context.Context, client *nemau.Client, zoneID string, interval time.Duration) error {
	var last error
	for {
		ref, err := client.DNS.VerifyZone(ctx, zoneID)
		switch {
		case nemau.HasReason(err, nemau.ReasonDNSZoneAlreadyVerified):
			return nil
		case err != nil && ctx.Err() != nil:
			return &verificationExpiredError{last: last}
		case err != nil:
			return err
		}

		_, err = client.WaitFor(ctx, ref)
		if err == nil {
			return nil
		}
		var failed *nemau.TaskFailedError
		switch {
		case errors.As(err, &failed):
			last = err
			tflog.Info(ctx, "DNS zone ownership not verified yet, retrying", map[string]any{
				"zone_id": zoneID, "error": err.Error(), "retry_in": interval.String(),
			})
		case ctx.Err() != nil:
			return &verificationExpiredError{last: last}
		default:
			return err
		}

		select {
		case <-ctx.Done():
			return &verificationExpiredError{last: last}
		case <-time.After(interval):
		}
	}
}

// verificationExpiredDetail tells what to check once the deadline is over,
// naming the record when the zone can still be read. It runs on the caller's
// context, not on the expired one.
func verificationExpiredDetail(ctx context.Context, client *nemau.Client, zoneID string, expired *verificationExpiredError) string {
	detail := expired.Error()
	if zone, err := client.DNS.GetZone(ctx, zoneID); err == nil && zone.Verification != nil {
		detail += fmt.Sprintf("\n\nThe platform looks for a %s record named %s holding %q. "+
			"Check that it is published at the domain's current DNS host "+
			"(dig TXT %s +short), then apply again; the zone stays pending until then. "+
			"A longer timeout helps when the DNS host is slow to publish.",
			zone.Verification.Type, zone.Verification.Name, zone.Verification.Value, zone.Verification.Name)
	}
	return detail
}
