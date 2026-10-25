package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

// ---------------------------------------------------------------------------
// Managed PostgreSQL database, read by id or by name
// ---------------------------------------------------------------------------

var (
	_ datasource.DataSource              = (*databaseDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*databaseDataSource)(nil)
)

func NewDatabaseDataSource() datasource.DataSource { return &databaseDataSource{} }

type databaseDataSource struct{ client *nemau.Client }

type databaseDataModel struct {
	ID                  types.String `tfsdk:"id"`
	Name                types.String `tfsdk:"name"`
	Description         types.String `tfsdk:"description"`
	Version             types.String `tfsdk:"version"`
	Flavor              types.String `tfsdk:"flavor"`
	StorageGB           types.Int64  `tfsdk:"storage_gb"`
	SubnetID            types.String `tfsdk:"subnet_id"`
	SecurityGroupIDs    types.List   `tfsdk:"security_group_ids"`
	AdminUsername       types.String `tfsdk:"admin_username"`
	BackupRetentionDays types.Int64  `tfsdk:"backup_retention_days"`
	MaintenanceWindow   types.Object `tfsdk:"maintenance_window"`
	Status              types.String `tfsdk:"status"`
	Address             types.String `tfsdk:"address"`
	Port                types.Int64  `tfsdk:"port"`
}

func (d *databaseDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_database"
}

func (d *databaseDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An existing managed PostgreSQL database, for referencing one Terraform does not manage. " +
			"Set exactly one of `id` and `name`. The administrator password is never readable: it is issued once, " +
			"at creation, to the resource that created the database.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "The database's public id. Set it, or `name`.",
			},
			"name": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "The database's name. It must match one database of the project. Set it, or `id`.",
			},
			"description":           schema.StringAttribute{Computed: true, MarkdownDescription: "Free text."},
			"version":               schema.StringAttribute{Computed: true, MarkdownDescription: "PostgreSQL major version."},
			"flavor":                schema.StringAttribute{Computed: true, MarkdownDescription: "Size from the region's offer."},
			"storage_gb":            schema.Int64Attribute{Computed: true, MarkdownDescription: "Storage in GB."},
			"subnet_id":             schema.StringAttribute{Computed: true, MarkdownDescription: "Subnet of the database."},
			"security_group_ids":    schema.ListAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "Security group ids that filter access."},
			"admin_username":        schema.StringAttribute{Computed: true, MarkdownDescription: "Administrator role name."},
			"backup_retention_days": schema.Int64Attribute{Computed: true, MarkdownDescription: "Days the backups are kept after deletion."},
			"maintenance_window": schema.SingleNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The UTC maintenance window.",
				Attributes: map[string]schema.Attribute{
					"weekday": schema.Int64Attribute{Computed: true, MarkdownDescription: "Day, 0 (Sunday) to 6."},
					"hour":    schema.Int64Attribute{Computed: true, MarkdownDescription: "Hour, 0 to 23, UTC."},
				},
			},
			"status":  schema.StringAttribute{Computed: true, MarkdownDescription: "Lifecycle state as the platform reports it."},
			"address": schema.StringAttribute{Computed: true, MarkdownDescription: "Host name or address clients connect to."},
			"port":    schema.Int64Attribute{Computed: true, MarkdownDescription: "TCP port clients connect to."},
		},
	}
}

func (d *databaseDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromDataSourceProvider(req, resp)
}

func (d *databaseDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config databaseDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	hasID := !config.ID.IsNull() && !config.ID.IsUnknown()
	hasName := !config.Name.IsNull() && !config.Name.IsUnknown()
	if hasID == hasName {
		resp.Diagnostics.AddError("Set exactly one of id and name", "The database data source needs one of id or name.")
		return
	}

	var db *nemau.Database
	if hasID {
		found, err := d.client.Databases.Get(ctx, config.ID.ValueString())
		if err != nil {
			resp.Diagnostics.AddError("Reading database failed", err.Error())
			return
		}
		db = found
	} else {
		matches, err := databasesNamed(ctx, d.client, config.Name.ValueString())
		if err != nil {
			resp.Diagnostics.AddError("Listing databases failed", err.Error())
			return
		}
		switch len(matches) {
		case 0:
			resp.Diagnostics.AddError("No such database", fmt.Sprintf("No database of the project is named %q.", config.Name.ValueString()))
			return
		case 1:
			db = &matches[0]
		default:
			resp.Diagnostics.AddError("Name is ambiguous", fmt.Sprintf("%d databases are named %q. Use id instead.", len(matches), config.Name.ValueString()))
			return
		}
	}

	window, diags := objectFromWindow(ctx, db.MaintenanceWindow)
	resp.Diagnostics.Append(diags...)
	state := databaseDataModel{
		ID: types.StringValue(db.ID), Name: types.StringValue(db.Name),
		Description: str(db.Description), Version: types.StringValue(db.Version),
		Flavor: types.StringValue(db.Flavor), StorageGB: types.Int64Value(int64(db.StorageGB)),
		SubnetID: types.StringValue(db.SubnetID), SecurityGroupIDs: strList(db.SecurityGroupIDs),
		AdminUsername: types.StringValue(db.AdminUsername), BackupRetentionDays: types.Int64Value(int64(db.BackupRetentionDays)),
		MaintenanceWindow: window, Status: types.StringValue(db.Status),
		Address: str(db.Address), Port: types.Int64Value(int64(db.Port)),
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// databasesNamed lists the project's databases with the given name. A deleted
// database keeps its name for a while; it is not a match.
func databasesNamed(ctx context.Context, c *nemau.Client, name string) ([]nemau.Database, error) {
	all, err := c.Databases.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []nemau.Database
	for _, db := range all {
		if db.Name == name && db.Status != statusDeleted {
			out = append(out, db)
		}
	}
	return out, nil
}
