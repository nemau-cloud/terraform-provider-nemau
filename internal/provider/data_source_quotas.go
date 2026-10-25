package provider

import (
	"context"
	"sort"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

// ---------------------------------------------------------------------------
// Quotas
// ---------------------------------------------------------------------------
//
// The capacity limits the platform checks writes against. A configuration can
// read them to size itself, or to fail early with its own message through a
// precondition, instead of stopping halfway through an apply on a refusal from
// the platform.

var (
	_ datasource.DataSource              = (*quotasDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*quotasDataSource)(nil)
)

func NewQuotasDataSource() datasource.DataSource { return &quotasDataSource{} }

type quotasDataSource struct{ client *nemau.Client }

type quotaDimensionEntry struct {
	Name      types.String `tfsdk:"name"`
	Scope     types.String `tfsdk:"scope"`
	Unit      types.String `tfsdk:"unit"`
	Used      types.Int64  `tfsdk:"used"`
	Reserved  types.Int64  `tfsdk:"reserved"`
	Limit     types.Int64  `tfsdk:"limit"`
	Available types.Int64  `tfsdk:"available"`
}

type quotasModel struct {
	Scope      types.String          `tfsdk:"scope"`
	ProjectID  types.String          `tfsdk:"project_id"`
	Dimensions []quotaDimensionEntry `tfsdk:"dimensions"`
}

func (d *quotasDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_quotas"
}

func (d *quotasDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The capacity limits applied to the configured credential.\n\n" +
			"A write is admitted only when the project and the organization that owns it both " +
			"have room. Without `scope`, each dimension is reported once, for the scope that " +
			"will refuse first: the one with the least headroom, which does not always have the " +
			"smallest limit.",
		Attributes: map[string]schema.Attribute{
			"scope": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Report a single scope, `project` or `tenant`, instead of " +
					"whichever one binds per dimension.",
			},
			"project_id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The project the credential resolved to, empty when it resolves to none.",
			},
			"dimensions": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "One entry per limited resource.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"name": schema.StringAttribute{
							Computed: true,
							MarkdownDescription: "What is limited: `instances`, `cores`, `ram`, `volumes`, `volumeSize`, " +
								"`snapshots`, `networks`, `subnets`, `floatingIps`, `securityGroups`, `loadBalancers` " +
								"or `objectStorage`.",
						},
						"scope": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "`project` or `tenant` (the organization).",
						},
						"unit": schema.StringAttribute{
							Computed: true,
							MarkdownDescription: "`count`, `MB`, `GB` or `bytes`. RAM is counted in MB " +
								"and volume size in GB, so one unit cannot be assumed for both.",
						},
						"used": schema.Int64Attribute{
							Computed:            true,
							MarkdownDescription: "Capacity in use, in `unit`.",
						},
						"reserved": schema.Int64Attribute{
							Computed: true,
							MarkdownDescription: "Capacity claimed by operations admitted but not yet " +
								"finished. The platform counts it against the limit.",
						},
						"limit": schema.Int64Attribute{
							Computed:            true,
							MarkdownDescription: "The limit, in `unit`.",
						},
						"available": schema.Int64Attribute{
							Computed:            true,
							MarkdownDescription: "`limit - used - reserved`, never negative.",
						},
					},
				},
			},
		},
	}
}

func (d *quotasDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromDataSourceProvider(req, resp)
}

func (d *quotasDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config quotasModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	quotas, err := d.client.Quotas.Get(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Cannot read the quotas", err.Error())
		return
	}

	config.ProjectID = types.StringValue(quotaProjectID(quotas))
	config.Dimensions = quotaEntries(quotas, config.Scope.ValueString())
	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}

// quotaProjectID reports the project the read resolved to, empty for a
// credential that resolves to none.
func quotaProjectID(quotas *nemau.QuotaList) string {
	if project := quotas.Scope(nemau.QuotaScopeProject); project != nil {
		return project.ProjectID
	}
	return ""
}

// quotaEntries flattens the reply. Without a scope filter, each dimension
// appears once, for the scope that will refuse first, so that a precondition
// on this data source reads the limit that actually applies.
func quotaEntries(quotas *nemau.QuotaList, scope string) []quotaDimensionEntry {
	entries := make([]quotaDimensionEntry, 0)
	for _, name := range quotaNames(quotas) {
		if scope == "" {
			if bindingScope, dimension := quotaBinding(quotas, name); dimension != nil {
				entries = append(entries, quotaEntry(bindingScope, dimension))
			}
			continue
		}
		for i := range quotas.Quotas {
			if quotas.Quotas[i].Scope != scope {
				continue
			}
			if dimension := quotas.Quotas[i].Dimension(name); dimension != nil {
				entries = append(entries, quotaEntry(quotas.Quotas[i].Scope, dimension))
			}
		}
	}
	return entries
}

func quotaEntry(scope string, dimension *nemau.QuotaDimension) quotaDimensionEntry {
	return quotaDimensionEntry{
		Name:      types.StringValue(dimension.Name),
		Scope:     types.StringValue(scope),
		Unit:      types.StringValue(dimension.Unit),
		Used:      types.Int64Value(dimension.Used),
		Reserved:  types.Int64Value(dimension.Reserved),
		Limit:     types.Int64Value(dimension.Limit),
		Available: types.Int64Value(dimension.Available()),
	}
}

// quotaBinding returns the scope that will refuse first and its dimension. The
// SDK's Binding answers with the dimension alone, and an entry that does not
// name its scope cannot be acted on in a precondition.
func quotaBinding(quotas *nemau.QuotaList, name string) (string, *nemau.QuotaDimension) {
	var scope string
	var binding *nemau.QuotaDimension
	for i := range quotas.Quotas {
		dimension := quotas.Quotas[i].Dimension(name)
		if dimension == nil {
			continue
		}
		if binding == nil || dimension.Available() < binding.Available() {
			scope, binding = quotas.Quotas[i].Scope, dimension
		}
	}
	return scope, binding
}

// quotaNames lists every dimension any scope reported, in a stable order so a
// plan does not churn between runs.
func quotaNames(quotas *nemau.QuotaList) []string {
	seen := map[string]bool{}
	var names []string
	for i := range quotas.Quotas {
		for _, dimension := range quotas.Quotas[i].Dimensions {
			if !seen[dimension.Name] {
				seen[dimension.Name] = true
				names = append(names, dimension.Name)
			}
		}
	}
	sort.Strings(names)
	return names
}
