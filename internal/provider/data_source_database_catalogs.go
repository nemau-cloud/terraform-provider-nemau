package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

// The database catalogs: the flavors and the PostgreSQL versions a region
// offers. They are read-only, so a configuration can pick a valid value for
// nemau_database without guessing the offer.

var (
	_ datasource.DataSource              = (*databaseFlavorsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*databaseFlavorsDataSource)(nil)
	_ datasource.DataSource              = (*databaseVersionsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*databaseVersionsDataSource)(nil)
)

var databaseFlavorAttrTypes = map[string]attr.Type{
	"name":      types.StringType,
	"vcpus":     types.Int64Type,
	"memory_mb": types.Int64Type,
}

// ---------------------------------------------------------------------------
// Database flavors
// ---------------------------------------------------------------------------

func NewDatabaseFlavorsDataSource() datasource.DataSource { return &databaseFlavorsDataSource{} }

type databaseFlavorsDataSource struct{ client *nemau.Client }

type databaseFlavorEntry struct {
	Name     types.String `tfsdk:"name"`
	VCPUs    types.Int64  `tfsdk:"vcpus"`
	MemoryMB types.Int64  `tfsdk:"memory_mb"`
}

type databaseFlavorsModel struct {
	Flavors []databaseFlavorEntry `tfsdk:"flavors"`
}

func (d *databaseFlavorsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_database_flavors"
}

func (d *databaseFlavorsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The database flavors the region offers, in the offer's order. Use a `name` as " +
			"`flavor` of `nemau_database`. The provider does not check the flavor at plan time: the platform " +
			"refuses a value the offer does not list, and a downgrade, at apply.",
		Attributes: map[string]schema.Attribute{
			"flavors": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The flavors.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"name": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "The flavor name, for `flavor` of `nemau_database`.",
						},
						"vcpus": schema.Int64Attribute{
							Computed:            true,
							MarkdownDescription: "Number of virtual CPUs.",
						},
						"memory_mb": schema.Int64Attribute{
							Computed:            true,
							MarkdownDescription: "Memory in MB.",
						},
					},
				},
			},
		},
	}
}

func (d *databaseFlavorsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromDataSourceProvider(req, resp)
}

func (d *databaseFlavorsDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	flavors, err := d.client.Databases.Flavors(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Cannot read the database flavors", err.Error())
		return
	}
	model := databaseFlavorsModel{Flavors: make([]databaseFlavorEntry, 0, len(flavors))}
	for _, f := range flavors {
		model.Flavors = append(model.Flavors, databaseFlavorEntry{
			Name:     types.StringValue(f.Name),
			VCPUs:    types.Int64Value(int64(f.VCPUs)),
			MemoryMB: types.Int64Value(int64(f.MemoryMB)),
		})
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}

// ---------------------------------------------------------------------------
// Database versions
// ---------------------------------------------------------------------------

func NewDatabaseVersionsDataSource() datasource.DataSource { return &databaseVersionsDataSource{} }

type databaseVersionsDataSource struct{ client *nemau.Client }

type databaseVersionsModel struct {
	Versions types.List `tfsdk:"versions"`
}

func (d *databaseVersionsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_database_versions"
}

func (d *databaseVersionsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The PostgreSQL versions the region offers. Use one as `version` of `nemau_database`.",
		Attributes: map[string]schema.Attribute{
			"versions": schema.ListAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "The versions, in the platform's order.",
			},
		},
	}
}

func (d *databaseVersionsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromDataSourceProvider(req, resp)
}

func (d *databaseVersionsDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	versions, err := d.client.Databases.Versions(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Cannot read the database versions", err.Error())
		return
	}
	model := databaseVersionsModel{Versions: strList(versions)}
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}
