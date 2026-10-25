package provider

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

// ---------------------------------------------------------------------------
// Managed PostgreSQL database
// ---------------------------------------------------------------------------

var (
	_ resource.Resource                   = (*databaseResource)(nil)
	_ resource.ResourceWithConfigure      = (*databaseResource)(nil)
	_ resource.ResourceWithImportState    = (*databaseResource)(nil)
	_ resource.ResourceWithValidateConfig = (*databaseResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*databaseResource)(nil)
)

func NewDatabaseResource() resource.Resource { return &databaseResource{} }

type databaseResource struct{ client *nemau.Client }

type databaseModel struct {
	ID                      types.String `tfsdk:"id"`
	Name                    types.String `tfsdk:"name"`
	Description             types.String `tfsdk:"description"`
	Version                 types.String `tfsdk:"version"`
	Flavor                  types.String `tfsdk:"flavor"`
	StorageGB               types.Int64  `tfsdk:"storage_gb"`
	SubnetID                types.String `tfsdk:"subnet_id"`
	SecurityGroupIDs        types.List   `tfsdk:"security_group_ids"`
	AdminUsername           types.String `tfsdk:"admin_username"`
	AdminPassword           types.String `tfsdk:"admin_password"`
	PasswordRotationTrigger types.String `tfsdk:"password_rotation_trigger"`
	BackupRetentionDays     types.Int64  `tfsdk:"backup_retention_days"`
	MaintenanceWindow       types.Object `tfsdk:"maintenance_window"`
	ResizeApplyAt           types.String `tfsdk:"resize_apply_at"`
	PurgeBackupsOnDestroy   types.Bool   `tfsdk:"purge_backups_on_destroy"`
	RestoreFrom             types.Object `tfsdk:"restore_from"`
	Status                  types.String `tfsdk:"status"`
	Address                 types.String `tfsdk:"address"`
	Port                    types.Int64  `tfsdk:"port"`
}

// Object shapes of the nested values. Their field names are the attribute names.
type windowModel struct {
	Weekday types.Int64 `tfsdk:"weekday"`
	Hour    types.Int64 `tfsdk:"hour"`
}

type restoreFromModel struct {
	SourceDatabaseID types.String `tfsdk:"source_database_id"`
	TargetTime       types.String `tfsdk:"target_time"`
}

var windowAttrTypes = map[string]attr.Type{"weekday": types.Int64Type, "hour": types.Int64Type}

// Limits of a database, as the platform checks them. They are repeated here so
// that a plan fails before any write; the platform's answer still decides.
const (
	dbStorageMinGB     = 10
	dbStorageMaxGB     = 2000
	dbRetentionMinDays = 1
	dbRetentionMaxDays = 35
	dbDescriptionMax   = 256
	dbMaxSecurityGroup = 5
)

var (
	dbNamePattern      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	dbAdminNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{2,31}$`)
)

func (r *databaseResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_database"
}

func (r *databaseResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A managed PostgreSQL database in a project subnet.\n\n" +
			"**The administrator password is shown once.** The platform keeps only a verifier of it. " +
			"`admin_password` holds the password issued by the create, by a change of " +
			"`password_rotation_trigger`, and nothing else: an import, or a refresh, cannot recover it. " +
			"The value is kept in the Terraform state, so use an encrypted remote backend and restrict who " +
			"can read the state. The attribute is marked sensitive, which hides it from plan and apply " +
			"output, not from the state file.\n\n" +
			"**Replacements destroy the data.** Changing `version`, `subnet_id`, `admin_username`, or " +
			"any field of `restore_from` replaces the database: the old one is deleted, with its " +
			"backups kept for their retention unless `purge_backups_on_destroy` is set. Protect the " +
			"resource with `lifecycle { prevent_destroy = true }` where that matters.\n\n" +
			"**Grow only.** `storage_gb` can only grow. `flavor` moves up the region's offer; the offer " +
			"is not readable through the API, so the platform refuses a downgrade at apply, not at plan. " +
			"A resize is applied at once by default; set `resize_apply_at` to `maintenance_window` to " +
			"defer it. A deferred resize is planned at its new values, and the state keeps them while the " +
			"platform reports the resize as pending.\n\n" +
			"**Restore.** `restore_from` creates a new database from a point in time of another one. The " +
			"source is never changed. The restore keeps the source's version, so a `version` other than the " +
			"source's is refused at plan time. The restore takes the source's backup retention and maintenance " +
			"window: when the configuration sets either, the provider applies it with one update after the restore, and a window " +
			"the configuration omits is cleared the same way. " +
			"After an import, the restored database has no `restore_from` in state: remove the block from the " +
			"configuration, or the next plan replaces the database.\n\n" +
			"`security_group_ids` cannot change on an existing database: the platform has no operation " +
			"for it, so the plan refuses the change.\n\n" +
			"Creating a database waits for the platform to provision it. Clients connect over TLS, with the " +
			"CA certificate that `nemau database ca-certificate` prints, as sslrootcert with sslmode=verify-full.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The database's public id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Name, 1 to 63 characters: a lowercase letter, then lowercase letters, digits " +
					"or hyphens. Changes in place.",
			},
			"description": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Free text, at most 256 characters, without control characters. Changes in place; empty clears it.",
			},
			"version": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "PostgreSQL major version from the region's offer. Required unless `restore_from` is set, " +
					"in which case the source's version is kept: a restore refuses any other value at plan time. " +
					"Changing it replaces the database.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown(), stringplanmodifier.RequiresReplace()},
			},
			"flavor": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Size from the region's offer. Changes in place as a resize, which only moves up the " +
					"offer; the platform refuses a downgrade.",
			},
			"storage_gb": schema.Int64Attribute{
				Required: true,
				MarkdownDescription: "Storage in GB, 10 to 2000. Changes in place as a resize; it can only grow, and a " +
					"smaller value is refused at plan time.",
			},
			"subnet_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Subnet the database is attached to. Changing it replaces the database.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"security_group_ids": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				MarkdownDescription: "Up to 5 security group ids that filter access to the database. Set at creation " +
					"only: a change on an existing database is refused at plan time.",
			},
			"admin_username": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Administrator role, 3 to 32 characters: a lowercase letter, then lowercase letters, " +
					"digits or underscores. `postgres`, and names starting with `pg_` or `nemau`, are reserved. " +
					"Changing it replaces the database.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"admin_password": schema.StringAttribute{
				Computed:  true,
				Sensitive: true,
				MarkdownDescription: "The administrator password. Known after the create, and after each change of " +
					"`password_rotation_trigger`; kept in the state. Empty after an import, where it cannot be recovered.",
			},
			"password_rotation_trigger": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Any value. Changing it rotates the administrator password, and the platform " +
					"issues a new one, shown in `admin_password`. Use `timestamp()` for a rotation on every apply, " +
					"or a counter you bump on purpose.",
			},
			"backup_retention_days": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Days the backups are kept after the database is deleted, 1 to 35. Changes in place. " +
					"Raising it is billed. Unset, the platform's value is read back.",
				PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"maintenance_window": schema.SingleNestedAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "The one UTC hour a week for minor updates and maintenance. Omitting the block means no " +
					"window: a window the platform sets is cleared at create, and removing the block from a configuration " +
					"clears the window on the next apply. Set the block to choose a window.",
				Attributes: map[string]schema.Attribute{
					"weekday": schema.Int64Attribute{
						Required:            true,
						MarkdownDescription: "Day, 0 for Sunday through 6 for Saturday, in UTC.",
					},
					"hour": schema.Int64Attribute{
						Required:            true,
						MarkdownDescription: "Hour, 0 to 23, in UTC.",
					},
				},
				PlanModifiers: []planmodifier.Object{objectplanmodifier.UseStateForUnknown()},
			},
			"resize_apply_at": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(nemau.ApplyNow),
				MarkdownDescription: "When a resize of `flavor` or `storage_gb` is applied: `now` (the default), which waits " +
					"for it, or `maintenance_window`, which does not wait.",
			},
			"purge_backups_on_destroy": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Erase the backups when the database is destroyed. Off by default: the backups " +
					"then survive for their retention period, billed by the gigabyte.",
			},
			"status": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Lifecycle state as the platform reports it.",
			},
			"address": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Host name or address clients connect to. Empty until the database is ready.",
			},
			"port": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "TCP port clients connect to.",
			},
		},
		Blocks: map[string]schema.Block{
			"restore_from": schema.SingleNestedBlock{
				MarkdownDescription: "Create this database as a point-in-time restore of another one, instead of an empty " +
					"database. Changing any field replaces this database (see the description above). The restore keeps " +
					"the source's version. Its `backup_retention_days` and `maintenance_window`, when the configuration " +
					"sets them and they differ from the source's, are applied with one update after the restore.",
				Attributes: map[string]schema.Attribute{
					"source_database_id": schema.StringAttribute{
						Required:            true,
						MarkdownDescription: "The public id of the database to restore. It is not changed.",
						PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
					},
					"target_time": schema.StringAttribute{
						Required: true,
						MarkdownDescription: "RFC 3339 point in time, in the past and inside the source's restorable window, " +
							"for example `2026-10-20T09:30:00Z`.",
						PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
					},
				},
			},
		},
	}
}

func (r *databaseResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromResourceProvider(req, resp)
}

func (r *databaseResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importByID(ctx, req, resp)
}

// ValidateConfig checks what can be checked without a round trip: the shape and
// range of each value. The offer (version, flavor) and the restore window are the
// platform's to judge.
func (r *databaseResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var c databaseModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &c)...)
	if resp.Diagnostics.HasError() {
		return
	}
	known := func(v types.String) bool { return !v.IsUnknown() && !v.IsNull() }

	if known(c.Name) && !dbNamePattern.MatchString(c.Name.ValueString()) {
		resp.Diagnostics.AddAttributeError(path.Root("name"), "Invalid database name",
			"name must be 1 to 63 characters: a lowercase letter, then lowercase letters, digits or hyphens.")
	}
	if known(c.AdminUsername) {
		name := c.AdminUsername.ValueString()
		switch {
		case !dbAdminNamePattern.MatchString(name):
			resp.Diagnostics.AddAttributeError(path.Root("admin_username"), "Invalid administrator name",
				"admin_username must be 3 to 32 characters: a lowercase letter, then lowercase letters, digits or underscores.")
		case name == "postgres" || strings.HasPrefix(name, "pg_") || strings.HasPrefix(name, "nemau"):
			resp.Diagnostics.AddAttributeError(path.Root("admin_username"), "Reserved administrator name",
				"postgres, and names starting with pg_ or nemau, are reserved.")
		}
	}
	if !c.Description.IsUnknown() && !c.Description.IsNull() && len(c.Description.ValueString()) > dbDescriptionMax {
		resp.Diagnostics.AddAttributeError(path.Root("description"), "Description too long",
			fmt.Sprintf("description must be at most %d bytes.", dbDescriptionMax))
	}
	if !c.StorageGB.IsUnknown() && !c.StorageGB.IsNull() {
		if gb := c.StorageGB.ValueInt64(); gb < dbStorageMinGB || gb > dbStorageMaxGB {
			resp.Diagnostics.AddAttributeError(path.Root("storage_gb"), "Storage out of range",
				fmt.Sprintf("storage_gb must be between %d and %d.", dbStorageMinGB, dbStorageMaxGB))
		}
	}
	if !c.BackupRetentionDays.IsUnknown() && !c.BackupRetentionDays.IsNull() {
		if d := c.BackupRetentionDays.ValueInt64(); d < dbRetentionMinDays || d > dbRetentionMaxDays {
			resp.Diagnostics.AddAttributeError(path.Root("backup_retention_days"), "Retention out of range",
				fmt.Sprintf("backup_retention_days must be between %d and %d.", dbRetentionMinDays, dbRetentionMaxDays))
		}
	}
	if !c.SecurityGroupIDs.IsUnknown() && !c.SecurityGroupIDs.IsNull() && len(c.SecurityGroupIDs.Elements()) > dbMaxSecurityGroup {
		resp.Diagnostics.AddAttributeError(path.Root("security_group_ids"), "Too many security groups",
			fmt.Sprintf("a database accepts at most %d security groups.", dbMaxSecurityGroup))
	}
	if known(c.ResizeApplyAt) {
		if v := c.ResizeApplyAt.ValueString(); v != nemau.ApplyNow && v != nemau.ApplyMaintenanceWindow {
			resp.Diagnostics.AddAttributeError(path.Root("resize_apply_at"), "Invalid apply time",
				"resize_apply_at must be now or maintenance_window.")
		}
	}
	if !c.MaintenanceWindow.IsUnknown() && !c.MaintenanceWindow.IsNull() {
		var w windowModel
		if diags := c.MaintenanceWindow.As(ctx, &w, basetypes.ObjectAsOptions{}); !diags.HasError() {
			if (w.Weekday.IsUnknown() || w.Weekday.IsNull() || w.Weekday.ValueInt64() < 0 || w.Weekday.ValueInt64() > 6) ||
				(w.Hour.IsUnknown() || w.Hour.IsNull() || w.Hour.ValueInt64() < 0 || w.Hour.ValueInt64() > 23) {
				if !w.Weekday.IsUnknown() && !w.Hour.IsUnknown() {
					resp.Diagnostics.AddAttributeError(path.Root("maintenance_window"), "Invalid maintenance window",
						"weekday must be 0 to 6 (Sunday is 0) and hour 0 to 23, in UTC.")
				}
			}
		}
	}

	restoreSet := !c.RestoreFrom.IsNull() && !c.RestoreFrom.IsUnknown()
	if restoreSet {
		var rf restoreFromModel
		if diags := c.RestoreFrom.As(ctx, &rf, basetypes.ObjectAsOptions{}); !diags.HasError() && known(rf.TargetTime) {
			if _, err := time.Parse(time.RFC3339, rf.TargetTime.ValueString()); err != nil {
				resp.Diagnostics.AddAttributeError(path.Root("restore_from").AtName("target_time"), "Invalid target time",
					"target_time must be an RFC 3339 time, for example 2026-10-20T09:30:00Z.")
			}
		}
	} else if c.Version.IsNull() {
		resp.Diagnostics.AddAttributeError(path.Root("version"), "Version is required",
			"version is required unless restore_from is set.")
	}
}

// ModifyPlan applies the rules that compare the plan with the state.
func (r *databaseResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	// A destroy: nothing to compare.
	if req.Plan.Raw.IsNull() {
		return
	}
	var plan databaseModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// The version the configuration sets, as opposed to the plan's: the plan
	// carries the state's version when the configuration omits it.
	var cfgVersion types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("version"), &cfgVersion)...)
	var cfgWindow types.Object
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("maintenance_window"), &cfgWindow)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// A create: a restore's version is checked against its source.
	if req.State.Raw.IsNull() {
		r.planRestore(ctx, plan, cfgVersion, resp)
		return
	}
	var state databaseModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// A plan that replaces the database is judged as a create: the new database
	// has its own storage and its own security groups, and its password is new.
	// A restore it creates is checked against its source as a create is.
	replacing := replacesDatabase(plan, state)
	if replacing {
		r.planRestore(ctx, plan, cfgVersion, resp)
	} else {
		if !plan.StorageGB.IsUnknown() && !state.StorageGB.IsNull() && plan.StorageGB.ValueInt64() < state.StorageGB.ValueInt64() {
			resp.Diagnostics.AddAttributeError(path.Root("storage_gb"), "Storage cannot shrink",
				fmt.Sprintf("storage_gb is %d and the database has %d GB. Storage only grows.",
					plan.StorageGB.ValueInt64(), state.StorageGB.ValueInt64()))
		}
		if !plan.SecurityGroupIDs.IsUnknown() && !plan.SecurityGroupIDs.Equal(state.SecurityGroupIDs) {
			resp.Diagnostics.AddAttributeError(path.Root("security_group_ids"), "Security groups cannot change",
				"security_group_ids is set at creation only. The platform has no operation to change it on an existing "+
					"database. Keep the value, or replace the database deliberately.")
		}
	}

	// The window is what the configuration sets. An omitted block means no window:
	// removing the block from a configuration that set one plans its removal,
	// which Update sends as a clear. A replacement creates the window afresh.
	switch {
	case replacing && cfgWindow.IsNull():
		plan.MaintenanceWindow = types.ObjectUnknown(windowAttrTypes)
	case !replacing && cfgWindow.IsNull() && !state.MaintenanceWindow.IsNull():
		plan.MaintenanceWindow = types.ObjectNull(windowAttrTypes)
	}

	// The password is only ever issued by a create or a rotation. Keep the stored
	// one unless the trigger changes or the database is replaced, in which case
	// the new one is unknown.
	if !replacing && plan.PasswordRotationTrigger.Equal(state.PasswordRotationTrigger) {
		plan.AdminPassword = state.AdminPassword
	} else {
		plan.AdminPassword = types.StringUnknown()
	}
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

// replacesDatabase reports whether a plan destroys the database and creates a
// new one: the attributes that require replacement differ from the state.
func replacesDatabase(plan, state databaseModel) bool {
	return !plan.Version.IsUnknown() && !plan.Version.Equal(state.Version) ||
		!plan.SubnetID.Equal(state.SubnetID) ||
		!plan.AdminUsername.Equal(state.AdminUsername) ||
		!plan.RestoreFrom.Equal(state.RestoreFrom)
}

// planRestore refuses a restore whose configured version is not the source's. A
// restore keeps the source's version, so a different one could only fail at
// apply. A configuration that omits version is not compared: the plan's value
// is then the state's, not a choice the user made.
func (r *databaseResource) planRestore(ctx context.Context, plan databaseModel, cfgVersion types.String, resp *resource.ModifyPlanResponse) {
	restore, restoring := restoreOf(ctx, plan.RestoreFrom, &resp.Diagnostics)
	if resp.Diagnostics.HasError() || !restoring || r.client == nil {
		return
	}
	if cfgVersion.IsNull() || plan.Version.IsUnknown() || plan.Version.IsNull() || restore.SourceDatabaseID.IsUnknown() || restore.SourceDatabaseID.IsNull() {
		return
	}
	source, err := r.client.Databases.Get(ctx, restore.SourceDatabaseID.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("restore_from").AtName("source_database_id"), "Reading the restore source failed", err.Error())
		return
	}
	if source.Version != plan.Version.ValueString() {
		resp.Diagnostics.AddAttributeError(path.Root("version"), "Restore keeps the source's version",
			fmt.Sprintf("version is %s, but the source runs PostgreSQL %s. A restore keeps the source's version: set version to %s, or leave it out.",
				plan.Version.ValueString(), source.Version, source.Version))
	}
}

func (r *databaseResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan databaseModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	security := stringsFromList(ctx, plan.SecurityGroupIDs, &resp.Diagnostics)
	// The window is read from the configuration: the plan's value of an omitted
	// block is the platform's, which is not known before the create.
	var cfgWindow types.Object
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("maintenance_window"), &cfgWindow)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var creds *nemau.DatabaseCredentials
	var err error
	restore, restoring := restoreOf(ctx, plan.RestoreFrom, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if restoring {
		target, _ := time.Parse(time.RFC3339, restore.TargetTime.ValueString())
		creds, err = r.client.Databases.Restore(ctx, restore.SourceDatabaseID.ValueString(), nemau.RestoreDatabaseRequest{
			Name: plan.Name.ValueString(), Description: plan.Description.ValueString(),
			TargetTime: target, Flavor: plan.Flavor.ValueString(), StorageGB: int(plan.StorageGB.ValueInt64()),
			SubnetID: plan.SubnetID.ValueString(), SecurityGroupIDs: security,
			AdminUsername: plan.AdminUsername.ValueString(),
		})
	} else {
		createReq := nemau.CreateDatabaseRequest{
			Name: plan.Name.ValueString(), Description: plan.Description.ValueString(),
			Version: plan.Version.ValueString(), Flavor: plan.Flavor.ValueString(),
			StorageGB: int(plan.StorageGB.ValueInt64()), SubnetID: plan.SubnetID.ValueString(),
			SecurityGroupIDs: security, AdminUsername: plan.AdminUsername.ValueString(),
		}
		if !plan.BackupRetentionDays.IsNull() && !plan.BackupRetentionDays.IsUnknown() {
			days := int(plan.BackupRetentionDays.ValueInt64())
			createReq.BackupRetentionDays = &days
		}
		window, diags := windowFromObject(ctx, cfgWindow)
		resp.Diagnostics.Append(diags...)
		createReq.MaintenanceWindow = window
		if resp.Diagnostics.HasError() {
			return
		}
		creds, err = r.client.Databases.Create(ctx, createReq)
	}
	if err != nil {
		resp.Diagnostics.AddError("Creating database was refused", refusalDetail(err))
		return
	}

	// The password exists only in this response. Nothing below can give it back,
	// so it is kept in plan before the wait.
	ref := &nemau.TaskRef{TaskID: creds.TaskID, Status: "pending"}
	task, err := r.client.WaitFor(ctx, ref)
	if err != nil {
		resp.Diagnostics.AddError("Creating database failed", taskErrorDetail(err, ref)+
			"\n\nThe administrator password issued by this request was not stored: the database id is unknown until "+
			"the task finishes. Once the database exists, import it and set a new password_rotation_trigger, or run "+
			"nemau database rotate-password.")
		return
	}
	id := ""
	if task.ResourceID != nil {
		id = *task.ResourceID
	}
	if id == "" {
		id = task.ResultString("database_id")
	}
	if id == "" {
		resp.Diagnostics.AddError("Creating database returned no id",
			fmt.Sprintf("Task %s finished without a database id. Contact support with this task id.", task.ID))
		return
	}

	// The id and the password are saved before the read: a failed read must not
	// leave the database out of state, which would orphan it and its password.
	plan.ID = types.StringValue(id)
	plan.AdminPassword = types.StringValue(creds.Password)
	resp.Diagnostics.Append(resp.State.Set(ctx, withoutUnknowns(plan))...)

	db, err := r.client.Databases.Get(ctx, id)
	if err != nil {
		resp.Diagnostics.AddError("Reading the new database failed", err.Error())
		return
	}
	if restoring {
		db = r.settleRestored(ctx, id, plan, cfgWindow, db, &resp.Diagnostics)
	} else if cfgWindow.IsNull() && db.MaintenanceWindow != nil {
		db = r.clearMaintenanceWindow(ctx, id, db, &resp.Diagnostics)
	}
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(fillDatabase(ctx, &plan, db)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *databaseResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state databaseModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	db, err := r.client.Databases.Get(ctx, state.ID.ValueString())
	if handleReadError(err, "database", resp) {
		return
	}
	// A deleted database is gone for Terraform: the next plan creates it again.
	if db.Status == statusDeleted {
		resp.State.RemoveResource(ctx)
		return
	}
	// The password is never returned again: the state keeps what it has.
	resp.Diagnostics.Append(fillDatabase(ctx, &state, db)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *databaseResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state databaseModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()

	// In place: name, description, retention and window go in one PATCH.
	patch := nemau.UpdateDatabaseRequest{}
	changed := false
	if !plan.Name.Equal(state.Name) {
		name := plan.Name.ValueString()
		patch.Name = &name
		changed = true
	}
	if !plan.Description.Equal(state.Description) {
		desc := plan.Description.ValueString()
		patch.Description = &desc
		changed = true
	}
	if !plan.BackupRetentionDays.IsUnknown() && !plan.BackupRetentionDays.Equal(state.BackupRetentionDays) {
		days := int(plan.BackupRetentionDays.ValueInt64())
		patch.BackupRetentionDays = &days
		changed = true
	}
	if !plan.MaintenanceWindow.IsUnknown() && !plan.MaintenanceWindow.Equal(state.MaintenanceWindow) {
		if plan.MaintenanceWindow.IsNull() {
			patch.ClearMaintenanceWindow = true
		} else {
			window, diags := windowFromObject(ctx, plan.MaintenanceWindow)
			resp.Diagnostics.Append(diags...)
			patch.MaintenanceWindow = window
		}
		changed = true
	}
	if resp.Diagnostics.HasError() {
		return
	}
	if changed {
		runAndWait(ctx, r.client, &resp.Diagnostics, "Updating database", func() (*nemau.TaskRef, error) {
			return r.client.Databases.Update(ctx, id, patch)
		})
		if resp.Diagnostics.HasError() {
			return
		}
	}

	// A resize: the flavor and the storage move together, as one task.
	if !plan.Flavor.Equal(state.Flavor) || !plan.StorageGB.Equal(state.StorageGB) {
		resize := nemau.ResizeDatabaseRequest{}
		if !plan.Flavor.Equal(state.Flavor) {
			resize.Flavor = plan.Flavor.ValueString()
		}
		if !plan.StorageGB.Equal(state.StorageGB) {
			gb := int(plan.StorageGB.ValueInt64())
			resize.StorageGB = &gb
		}
		if plan.ResizeApplyAt.ValueString() == nemau.ApplyMaintenanceWindow {
			resize.ApplyAt = nemau.ApplyMaintenanceWindow
			if _, err := r.client.Databases.Resize(ctx, id, resize); err != nil {
				resp.Diagnostics.AddError("Resizing database was refused", refusalDetail(err))
				return
			}
		} else {
			resize.ApplyAt = nemau.ApplyNow
			runAndWait(ctx, r.client, &resp.Diagnostics, "Resizing database", func() (*nemau.TaskRef, error) {
				return r.client.Databases.Resize(ctx, id, resize)
			})
			if resp.Diagnostics.HasError() {
				return
			}
		}
	}

	// A new trigger value rotates the password. The new one is kept in state.
	if !plan.PasswordRotationTrigger.Equal(state.PasswordRotationTrigger) {
		creds, err := r.client.Databases.RotatePassword(ctx, id)
		if err != nil {
			resp.Diagnostics.AddError("Rotating the administrator password was refused", refusalDetail(err))
			return
		}
		// The new password exists only in this response. It is kept in state
		// before the task is waited for, so that a failed task does not lose it.
		// The state holds no unknown values, so the prior state is the base.
		kept := state
		kept.AdminPassword = types.StringValue(creds.Password)
		kept.PasswordRotationTrigger = plan.PasswordRotationTrigger
		resp.Diagnostics.Append(resp.State.Set(ctx, &kept)...)

		ref := &nemau.TaskRef{TaskID: creds.TaskID, Status: "pending"}
		if _, err := r.client.WaitFor(ctx, ref); err != nil {
			resp.Diagnostics.AddError("Rotating the administrator password failed", taskErrorDetail(err, ref))
			return
		}
		plan.AdminPassword = types.StringValue(creds.Password)
	} else {
		plan.AdminPassword = state.AdminPassword
	}

	db, err := r.client.Databases.Get(ctx, id)
	if err != nil {
		resp.Diagnostics.AddError("Reading the database failed", err.Error())
		return
	}
	plan.ID = state.ID
	plan.RestoreFrom = state.RestoreFrom
	resp.Diagnostics.Append(fillDatabase(ctx, &plan, db)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *databaseResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state databaseModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	purge := !state.PurgeBackupsOnDestroy.IsNull() && state.PurgeBackupsOnDestroy.ValueBool()
	deleteAndWait(ctx, r.client, &resp.Diagnostics, "database", func() (*nemau.TaskRef, error) {
		return r.client.Databases.Delete(ctx, state.ID.ValueString(), purge)
	})
}

// settleRestored applies the backup retention and the maintenance window of the
// configuration to a restored database. A restore takes its settings from the
// source, so a value that differs is set with one update after the restore. It
// returns the database as it stands once the update has run.
func (r *databaseResource) settleRestored(ctx context.Context, id string, plan databaseModel, cfgWindow types.Object, db *nemau.Database, diags *diag.Diagnostics) *nemau.Database {
	var patch nemau.UpdateDatabaseRequest
	changed := false
	if !plan.BackupRetentionDays.IsUnknown() && !plan.BackupRetentionDays.IsNull() &&
		int(plan.BackupRetentionDays.ValueInt64()) != db.BackupRetentionDays {
		days := int(plan.BackupRetentionDays.ValueInt64())
		patch.BackupRetentionDays = &days
		changed = true
	}
	window, d := windowFromObject(ctx, cfgWindow)
	diags.Append(d...)
	if diags.HasError() {
		return db
	}
	if window != nil && (db.MaintenanceWindow == nil || *db.MaintenanceWindow != *window) {
		patch.MaintenanceWindow = window
		changed = true
	} else if window == nil && db.MaintenanceWindow != nil {
		patch.ClearMaintenanceWindow = true
		changed = true
	}
	if !changed {
		return db
	}
	runAndWait(ctx, r.client, diags, "Applying the settings of the restored database", func() (*nemau.TaskRef, error) {
		return r.client.Databases.Update(ctx, id, patch)
	})
	if diags.HasError() {
		return db
	}
	updated, err := r.client.Databases.Get(ctx, id)
	if err != nil {
		diags.AddError("Reading the restored database failed", err.Error())
		return db
	}
	return updated
}

// clearMaintenanceWindow removes the window the platform set on a new database
// whose configuration omits the block. It returns the database as it stands
// afterwards.
func (r *databaseResource) clearMaintenanceWindow(ctx context.Context, id string, db *nemau.Database, diags *diag.Diagnostics) *nemau.Database {
	runAndWait(ctx, r.client, diags, "Clearing the maintenance window", func() (*nemau.TaskRef, error) {
		return r.client.Databases.Update(ctx, id, nemau.UpdateDatabaseRequest{ClearMaintenanceWindow: true})
	})
	if diags.HasError() {
		return db
	}
	updated, err := r.client.Databases.Get(ctx, id)
	if err != nil {
		diags.AddError("Reading the database failed", err.Error())
		return db
	}
	return updated
}

// statusDeleted is the status of a database the platform has removed. Its
// backups may still exist, but the database is not part of the project.
const statusDeleted = "deleted"

// withoutUnknowns returns the model with each unknown attribute set to null. A
// state holds no unknown value, so a value saved before the platform's answer
// comes in is made of what is known.
func withoutUnknowns(m databaseModel) *databaseModel {
	v := reflect.ValueOf(&m).Elem()
	for i := 0; i < v.NumField(); i++ {
		field := v.Field(i)
		if value, ok := field.Interface().(attr.Value); ok && value.IsUnknown() {
			field.Set(reflect.Zero(field.Type()))
		}
	}
	return &m
}

// fillDatabase writes what the platform reports into the model. Values the
// platform never returns (the password, restore_from, the rotation trigger, the
// apply time, the purge flag) are left as they are.
func fillDatabase(ctx context.Context, m *databaseModel, db *nemau.Database) diag.Diagnostics {
	var diags diag.Diagnostics
	m.Name = types.StringValue(db.Name)
	m.Description = optionalString(db.Description, m.Description)
	m.Version = types.StringValue(db.Version)
	// A resize the platform has accepted but not applied reports the values it
	// will move to. While those match the model, the model's values stand: the
	// current values are the ones the resize replaces, and writing them would
	// make the apply's result differ from the plan.
	pending := db.PendingResize
	if pending == nil || pending.Flavor == "" || m.Flavor.IsUnknown() || m.Flavor.ValueString() != pending.Flavor {
		m.Flavor = types.StringValue(db.Flavor)
	}
	if pending == nil || pending.StorageGB == 0 || m.StorageGB.IsUnknown() || m.StorageGB.IsNull() ||
		m.StorageGB.ValueInt64() != int64(pending.StorageGB) {
		m.StorageGB = types.Int64Value(int64(db.StorageGB))
	}
	m.SubnetID = types.StringValue(db.SubnetID)
	m.SecurityGroupIDs = optionalStrList(db.SecurityGroupIDs, m.SecurityGroupIDs)
	m.AdminUsername = types.StringValue(db.AdminUsername)
	m.BackupRetentionDays = types.Int64Value(int64(db.BackupRetentionDays))
	window, d := objectFromWindow(ctx, db.MaintenanceWindow)
	diags.Append(d...)
	m.MaintenanceWindow = window
	m.Status = types.StringValue(db.Status)
	m.Address = types.StringValue(db.Address)
	m.Port = types.Int64Value(int64(db.Port))
	if m.ResizeApplyAt.IsUnknown() || m.ResizeApplyAt.IsNull() {
		m.ResizeApplyAt = types.StringValue(nemau.ApplyNow)
	}
	return diags
}

// optionalString keeps a null in state when the configuration left the value
// unset and the platform reports none.
func optionalString(value string, prior types.String) types.String {
	if value == "" && prior.IsNull() {
		return prior
	}
	return types.StringValue(value)
}

// optionalStrList is optionalString for a list.
func optionalStrList(values []string, prior types.List) types.List {
	if len(values) == 0 && prior.IsNull() {
		return prior
	}
	return strList(values)
}

// windowFromObject reads the maintenance_window attribute. Unset is nil.
func windowFromObject(ctx context.Context, obj types.Object) (*nemau.DatabaseMaintenanceWindow, diag.Diagnostics) {
	var diags diag.Diagnostics
	if obj.IsNull() || obj.IsUnknown() {
		return nil, diags
	}
	var w windowModel
	diags.Append(obj.As(ctx, &w, basetypes.ObjectAsOptions{})...)
	if diags.HasError() {
		return nil, diags
	}
	return &nemau.DatabaseMaintenanceWindow{Weekday: int(w.Weekday.ValueInt64()), Hour: int(w.Hour.ValueInt64())}, diags
}

// objectFromWindow builds the maintenance_window attribute from the API value.
func objectFromWindow(ctx context.Context, w *nemau.DatabaseMaintenanceWindow) (types.Object, diag.Diagnostics) {
	var diags diag.Diagnostics
	if w == nil {
		return types.ObjectNull(windowAttrTypes), diags
	}
	obj, d := types.ObjectValueFrom(ctx, windowAttrTypes, windowModel{
		Weekday: types.Int64Value(int64(w.Weekday)), Hour: types.Int64Value(int64(w.Hour)),
	})
	diags.Append(d...)
	return obj, diags
}

// restoreOf reads the restore_from block. The second result is false when the
// block is not set.
func restoreOf(ctx context.Context, obj types.Object, diags *diag.Diagnostics) (restoreFromModel, bool) {
	if obj.IsNull() || obj.IsUnknown() {
		return restoreFromModel{}, false
	}
	var rf restoreFromModel
	diags.Append(obj.As(ctx, &rf, basetypes.ObjectAsOptions{})...)
	return rf, true
}
