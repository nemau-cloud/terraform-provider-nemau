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
// Instance group, read by id or by name
// ---------------------------------------------------------------------------

var (
	_ datasource.DataSource              = (*instanceGroupDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*instanceGroupDataSource)(nil)
)

func NewInstanceGroupDataSource() datasource.DataSource { return &instanceGroupDataSource{} }

type instanceGroupDataSource struct{ client *nemau.Client }

type instanceGroupDataModel struct {
	ID              types.String `tfsdk:"id"`
	Name            types.String `tfsdk:"name"`
	NetworkID       types.String `tfsdk:"network_id"`
	SubnetID        types.String `tfsdk:"subnet_id"`
	Status          types.String `tfsdk:"status"`
	MinSize         types.Int64  `tfsdk:"min_size"`
	MaxSize         types.Int64  `tfsdk:"max_size"`
	DesiredCapacity types.Int64  `tfsdk:"desired_capacity"`
	InstanceCount   types.Int64  `tfsdk:"instance_count"`
	TemplateVersion types.Int64  `tfsdk:"template_version"`
	HealthCheck     types.String `tfsdk:"health_check"`
	SecurityGroups  types.List   `tfsdk:"security_group_ids"`
	InstanceIDs     types.List   `tfsdk:"instance_ids"`
	ImageID         types.String `tfsdk:"image_id"`
	FlavorID        types.String `tfsdk:"flavor_id"`
	UserDataPresent types.Bool   `tfsdk:"user_data_present"`
}

func (d *instanceGroupDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_instance_group"
}

func (d *instanceGroupDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An existing instance group, for referencing one Terraform does not manage. Set exactly " +
			"one of `id` and `name`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Optional: true, Computed: true,
				MarkdownDescription: "The group's public id. Set it, or `name`.",
			},
			"name": schema.StringAttribute{
				Optional: true, Computed: true,
				MarkdownDescription: "The group's name. It must match one group of the project. Set it, or `id`.",
			},
			"network_id":       schema.StringAttribute{Computed: true, MarkdownDescription: "Network of the members."},
			"subnet_id":        schema.StringAttribute{Computed: true, MarkdownDescription: "Subnet of the members."},
			"status":           schema.StringAttribute{Computed: true, MarkdownDescription: "State of the group."},
			"min_size":         schema.Int64Attribute{Computed: true, MarkdownDescription: "Minimum members."},
			"max_size":         schema.Int64Attribute{Computed: true, MarkdownDescription: "Maximum members."},
			"desired_capacity": schema.Int64Attribute{Computed: true, MarkdownDescription: "Members the group should run now."},
			"instance_count":   schema.Int64Attribute{Computed: true, MarkdownDescription: "Current number of members."},
			"template_version": schema.Int64Attribute{Computed: true, MarkdownDescription: "Version of the launch template."},
			"health_check":     schema.StringAttribute{Computed: true, MarkdownDescription: "Health check in use."},
			"security_group_ids": schema.ListAttribute{
				Computed: true, ElementType: types.StringType,
				MarkdownDescription: "Security group ids of the members.",
			},
			"instance_ids": schema.ListAttribute{
				Computed: true, ElementType: types.StringType,
				MarkdownDescription: "Ids of the current members.",
			},
			"image_id":          schema.StringAttribute{Computed: true, MarkdownDescription: "Image of the launch template."},
			"flavor_id":         schema.StringAttribute{Computed: true, MarkdownDescription: "Flavor of the launch template, when it has one."},
			"user_data_present": schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the launch template carries a cloud-init document. The document itself is never returned."},
		},
	}
}

func (d *instanceGroupDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromDataSourceProvider(req, resp)
}

func (d *instanceGroupDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config instanceGroupDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	hasID := !config.ID.IsNull() && !config.ID.IsUnknown()
	hasName := !config.Name.IsNull() && !config.Name.IsUnknown()
	if hasID == hasName {
		resp.Diagnostics.AddError("Set exactly one of id and name", "The instance group data source needs one of id or name.")
		return
	}

	id := config.ID.ValueString()
	if !hasID {
		all, err := d.client.InstanceGroups.List(ctx)
		if err != nil {
			resp.Diagnostics.AddError("Listing instance groups failed", err.Error())
			return
		}
		var matches []nemau.InstanceGroup
		for _, g := range all {
			if g.Name == config.Name.ValueString() {
				matches = append(matches, g)
			}
		}
		switch len(matches) {
		case 0:
			resp.Diagnostics.AddError("No such instance group", fmt.Sprintf("No instance group of the project is named %q.", config.Name.ValueString()))
			return
		case 1:
			id = matches[0].ID
		default:
			resp.Diagnostics.AddError("Name is ambiguous", fmt.Sprintf("%d instance groups are named %q. Use id instead.", len(matches), config.Name.ValueString()))
			return
		}
	}

	g, err := d.client.InstanceGroups.Get(ctx, id)
	if err != nil {
		resp.Diagnostics.AddError("Reading instance group failed", err.Error())
		return
	}
	members := make([]string, 0, len(g.Members))
	for _, m := range g.Members {
		members = append(members, m.InstanceID)
	}
	state := instanceGroupDataModel{
		ID: types.StringValue(g.ID), Name: types.StringValue(g.Name),
		NetworkID: types.StringValue(g.LaunchTemplate.NetworkID), SubnetID: types.StringValue(g.LaunchTemplate.SubnetID),
		Status: types.StringValue(g.Status), MinSize: types.Int64Value(int64(g.MinSize)),
		MaxSize: types.Int64Value(int64(g.MaxSize)), DesiredCapacity: types.Int64Value(int64(g.DesiredCapacity)),
		InstanceCount: types.Int64Value(int64(g.InstanceCount)), TemplateVersion: types.Int64Value(int64(g.TemplateVersion)),
		HealthCheck: types.StringValue(g.HealthCheck), SecurityGroups: strList(g.LaunchTemplate.SecurityGroupIDs),
		InstanceIDs: strList(members), ImageID: types.StringValue(g.LaunchTemplate.ImageID),
		FlavorID: str(g.LaunchTemplate.FlavorID), UserDataPresent: types.BoolValue(g.LaunchTemplate.UserDataPresent),
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
