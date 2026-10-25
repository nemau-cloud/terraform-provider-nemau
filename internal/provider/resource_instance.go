package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
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
	BackupPolicy   types.Object `tfsdk:"backup_policy"`
	UserData       types.String `tfsdk:"user_data"`
}

// backupPolicyAttrTypes is the object type of the backup_policy block. A null
// object means no scheduled backups.
var backupPolicyAttrTypes = map[string]attr.Type{
	"schedule":       types.StringType,
	"hour":           types.Int64Type,
	"weekday":        types.Int64Type,
	"retention_days": types.Int64Type,
}

// backupPolicyModel is the content of the backup_policy block. Its fields are
// framework values, not plain ints, so an unknown value from another resource
// survives plan time.
type backupPolicyModel struct {
	Schedule      types.String `tfsdk:"schedule"`
	Hour          types.Int64  `tfsdk:"hour"`
	Weekday       types.Int64  `tfsdk:"weekday"`
	RetentionDays types.Int64  `tfsdk:"retention_days"`
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
			"The size (`vcpus`, `ram_mb`, `disk_gb`), the security groups, the power state " +
			"(`started`) and the `backup_policy` change in place. The image, flavor, network, " +
			"service level, SSH key and `user_data` force a replacement.\n\n" +
			"Only the system disk is backed up. Block volumes attached to the instance are not, " +
			"and a database needs its own dump for a consistent copy.",
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
			"user_data": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "cloud-init document run at the instance's first boot: it must start " +
					"with `#cloud-config`, `#!` or a MIME multipart header, be UTF-8 without NUL bytes, and be " +
					"at most 16 KiB. The platform seals it, never returns it, and runs it only at creation, " +
					"so a change forces a replacement. The state keeps the value from the configuration, in " +
					"clear: protect the state file. When the platform reports a different SHA-256 (the instance " +
					"was made with another document, outside Terraform), the value is dropped from the state, " +
					"which plans a replacement. After `terraform import` the value is unknown: the state holds " +
					"`null`, and a configuration that sets `user_data` then plans a replacement.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"addresses": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Known IP addresses. Empty until the guest has reported its " +
					"network configuration.",
			},
		},
		Blocks: map[string]schema.Block{
			"backup_policy": schema.SingleNestedBlock{
				MarkdownDescription: "Scheduled snapshots of the system disk. Each one is deleted when its " +
					"`retention_days` have passed. Remove the block to stop scheduled backups: the " +
					"snapshots already taken keep their expiry. Manual snapshots never expire. " +
					"Changes in place; a change sends the whole policy.",
				Attributes: map[string]schema.Attribute{
					"schedule": schema.StringAttribute{
						Required:            true,
						MarkdownDescription: "`daily` or `weekly`. To stop backups, remove the block rather than setting `none`.",
					},
					"hour": schema.Int64Attribute{
						Optional: true, Computed: true,
						Default:             int64default.StaticInt64(2),
						MarkdownDescription: "UTC hour the backup starts, 0 to 23. Defaults to `2`.",
					},
					"weekday": schema.Int64Attribute{
						Optional: true, Computed: true,
						Default: int64default.StaticInt64(0),
						MarkdownDescription: "Day of the week, 0 (Sunday) to 6 (Saturday). Only `weekly` uses it; " +
							"the platform ignores it for `daily`. Defaults to `0`.",
					},
					"retention_days": schema.Int64Attribute{
						Required:            true,
						MarkdownDescription: "Days each scheduled snapshot is kept, 1 to 35.",
					},
				},
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
	validateBackupPolicyConfig(ctx, config.BackupPolicy, &resp.Diagnostics)
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
			UserData:       plan.UserData.ValueString(),
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
			r.saveCreated(ctx, &plan, resp)
			return
		}
	}

	// The backup policy is not part of the create request: it is written as a
	// PATCH once the instance exists.
	if !plan.BackupPolicy.IsNull() {
		update := backupUpdate(ctx, plan.BackupPolicy, &resp.Diagnostics)
		if resp.Diagnostics.HasError() {
			r.saveCreated(ctx, &plan, resp)
			return
		}
		runAndWait(ctx, r.client, &resp.Diagnostics, "Backup policy", func() (*nemau.TaskRef, error) {
			return r.client.Compute.UpdateInstance(ctx, id, update)
		})
		if resp.Diagnostics.HasError() {
			plan.BackupPolicy = types.ObjectNull(backupPolicyAttrTypes)
			r.saveCreated(ctx, &plan, resp)
			return
		}
	}

	r.refresh(ctx, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// saveCreated writes the state of an instance that Create has already made,
// although a later step failed. Without it Terraform forgets the instance and
// the next apply creates a second one. An error with state set is recorded as
// tainted, so the next apply replaces the instance. The refresh runs first, so
// the state holds what the platform reports rather than what was planned.
func (r *instanceResource) saveCreated(ctx context.Context, plan *instanceModel, resp *resource.CreateResponse) {
	r.refresh(ctx, plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
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
	if userDataDrifted(state.UserData, instance.UserDataSHA256) {
		state.UserData = types.StringNull()
	}
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
	if !plan.BackupPolicy.Equal(state.BackupPolicy) {
		backup := backupUpdate(ctx, plan.BackupPolicy, &resp.Diagnostics)
		if resp.Diagnostics.HasError() {
			return
		}
		update.BackupSchedule = backup.BackupSchedule
		update.BackupHour = backup.BackupHour
		update.BackupWeekday = backup.BackupWeekday
		update.BackupRetentionDays = backup.BackupRetentionDays
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

// userDataFingerprint is the hex SHA-256 the platform reports for a document. It
// hashes the document as the platform keeps it: a leading byte order mark is
// removed first, as the platform does before it checks and seals the document.
func userDataFingerprint(document string) string {
	sum := sha256.Sum256([]byte(strings.TrimPrefix(document, "\uFEFF")))
	return hex.EncodeToString(sum[:])
}

// userDataDrifted reports a configured document that the platform does not
// hold under the same fingerprint. Nothing is drift when there is nothing to
// compare: a null or unknown value (never set, or unknown after an import), an
// empty value (the SDK omits it, so the platform holds no fingerprint), or a
// platform that reports no fingerprint. The last case is not treated as drift
// on purpose: a read that loses the fingerprint must not replace a live
// instance.
func userDataDrifted(value types.String, platformSHA256 string) bool {
	if value.IsNull() || value.IsUnknown() || value.ValueString() == "" || platformSHA256 == "" {
		return false
	}
	return platformSHA256 != userDataFingerprint(value.ValueString())
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
	model.BackupPolicy = backupPolicyValue(instance)
}

// backupPolicyValue maps the platform's policy onto the block. A schedule of
// none, or no schedule at all, is a null block: it matches a configuration
// without the block, so there is no diff to chase. A missing hour or weekday
// is the platform default, and a missing retention is left null so that the
// diff shows it.
func backupPolicyValue(instance *nemau.Instance) types.Object {
	if instance.BackupSchedule != nemau.BackupDaily && instance.BackupSchedule != nemau.BackupWeekly {
		return types.ObjectNull(backupPolicyAttrTypes)
	}
	hour, weekday := int64(2), int64(0)
	if instance.BackupHour != nil {
		hour = int64(*instance.BackupHour)
	}
	if instance.BackupWeekday != nil {
		weekday = int64(*instance.BackupWeekday)
	}
	retention := types.Int64Null()
	if instance.BackupRetentionDays != nil {
		retention = types.Int64Value(int64(*instance.BackupRetentionDays))
	}
	return types.ObjectValueMust(backupPolicyAttrTypes, map[string]attr.Value{
		"schedule":       types.StringValue(instance.BackupSchedule),
		"hour":           types.Int64Value(hour),
		"weekday":        types.Int64Value(weekday),
		"retention_days": retention,
	})
}

// backupUpdate returns the PATCH fields that make the platform's policy match
// the block. A null block turns scheduling off and sends only the schedule. A
// set block sends all four fields, so an hour or weekday left over from an
// earlier policy never survives a change.
func backupUpdate(ctx context.Context, block types.Object, diags *diag.Diagnostics) nemau.Update {
	if block.IsNull() {
		return nemau.Update{BackupSchedule: nemau.Set(nemau.BackupNone)}
	}
	var m backupPolicyModel
	diags.Append(block.As(ctx, &m, basetypes.ObjectAsOptions{})...)
	if diags.HasError() {
		return nemau.Update{}
	}
	return nemau.Update{
		BackupSchedule:      nemau.Set(m.Schedule.ValueString()),
		BackupHour:          nemau.Set(int(m.Hour.ValueInt64())),
		BackupWeekday:       nemau.Set(int(m.Weekday.ValueInt64())),
		BackupRetentionDays: nemau.Set(int(m.RetentionDays.ValueInt64())),
	}
}

// validateBackupPolicyConfig checks the block at plan time with the same ranges
// as the API. Unknown values are skipped: they are checked at apply time.
func validateBackupPolicyConfig(ctx context.Context, block types.Object, diags *diag.Diagnostics) {
	if block.IsNull() || block.IsUnknown() {
		return
	}
	var m backupPolicyModel
	diags.Append(block.As(ctx, &m, basetypes.ObjectAsOptions{UnhandledUnknownAsEmpty: true})...)
	if diags.HasError() {
		return
	}
	if v := m.Schedule; !v.IsUnknown() && v.ValueString() != nemau.BackupDaily && v.ValueString() != nemau.BackupWeekly {
		diags.AddAttributeError(path.Root("backup_policy").AtName("schedule"), "Unsupported backup schedule",
			"Use `daily` or `weekly`. To stop scheduled backups, remove the `backup_policy` block.")
	}
	checkRange(diags, "hour", m.Hour, 0, 23)
	checkRange(diags, "weekday", m.Weekday, 0, 6)
	checkRange(diags, "retention_days", m.RetentionDays, 1, 35)
}

func checkRange(diags *diag.Diagnostics, name string, v types.Int64, lo, hi int64) {
	if v.IsUnknown() || v.IsNull() {
		return
	}
	if n := v.ValueInt64(); n < lo || n > hi {
		diags.AddAttributeError(path.Root("backup_policy").AtName(name), "Value out of range",
			fmt.Sprintf("`%s` must be between %d and %d.", name, lo, hi))
	}
}
