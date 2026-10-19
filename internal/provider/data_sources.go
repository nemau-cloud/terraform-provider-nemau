package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

// The data sources here are read-only lookups of things a configuration needs
// but does not manage: images, flavors, regions, existing networks, a running
// instance, the supported Kubernetes versions, and a cluster's kubeconfig.

// ---------------------------------------------------------------------------
// Images
// ---------------------------------------------------------------------------

var (
	_ datasource.DataSource              = (*imagesDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*imagesDataSource)(nil)
)

func NewImagesDataSource() datasource.DataSource { return &imagesDataSource{} }

type imagesDataSource struct{ client *nemau.Client }

type imageEntry struct {
	ID         types.String `tfsdk:"id"`
	Name       types.String `tfsdk:"name"`
	OSName     types.String `tfsdk:"os_name"`
	Status     types.String `tfsdk:"status"`
	DiskFormat types.String `tfsdk:"disk_format"`
}

type imagesModel struct {
	NameFilter types.String `tfsdk:"name"`
	Images     []imageEntry `tfsdk:"images"`
}

func (d *imagesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_images"
}

func (d *imagesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The bootable images available.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Keep only the images whose name matches this value exactly.",
			},
			"images": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The matching images.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id":          schema.StringAttribute{Computed: true, MarkdownDescription: "Image id, the value `image_id` takes."},
						"name":        schema.StringAttribute{Computed: true, MarkdownDescription: "Image name."},
						"os_name":     schema.StringAttribute{Computed: true, MarkdownDescription: "Operating system of the image."},
						"status":      schema.StringAttribute{Computed: true, MarkdownDescription: "Status as reported by the platform."},
						"disk_format": schema.StringAttribute{Computed: true, MarkdownDescription: "Disk format of the image."},
					},
				},
			},
		},
	}
}

func (d *imagesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromDataSourceProvider(req, resp)
}

func (d *imagesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config imagesModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	images, err := d.client.Compute.ListImages(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Cannot read the images", err.Error())
		return
	}
	config.Images = make([]imageEntry, 0, len(images))
	for _, image := range images {
		if !config.NameFilter.IsNull() && image.Name != config.NameFilter.ValueString() {
			continue
		}
		config.Images = append(config.Images, imageEntry{
			ID:         types.StringValue(image.ID),
			Name:       types.StringValue(image.Name),
			OSName:     str(image.OSName),
			Status:     str(image.Status),
			DiskFormat: str(image.DiskFormat),
		})
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}

// ---------------------------------------------------------------------------
// Flavors
// ---------------------------------------------------------------------------

var (
	_ datasource.DataSource              = (*flavorsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*flavorsDataSource)(nil)
)

func NewFlavorsDataSource() datasource.DataSource { return &flavorsDataSource{} }

type flavorsDataSource struct{ client *nemau.Client }

type flavorEntry struct {
	ID           types.String `tfsdk:"id"`
	Name         types.String `tfsdk:"name"`
	VCPUs        types.Int64  `tfsdk:"vcpus"`
	RAMMB        types.Int64  `tfsdk:"ram_mb"`
	DiskGB       types.Int64  `tfsdk:"disk_gb"`
	ServiceLevel types.String `tfsdk:"service_level"`
}

type flavorsModel struct {
	NameFilter types.String  `tfsdk:"name"`
	Flavors    []flavorEntry `tfsdk:"flavors"`
}

func (d *flavorsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_flavors"
}

func (d *flavorsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The organization's instance flavors.\n\n" +
			"Requires the `compute-flavors` organization feature; without it the API answers " +
			"403 and this data source fails.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Keep only the flavors whose name matches this value exactly.",
			},
			"flavors": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The matching flavors.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id":            schema.StringAttribute{Computed: true, MarkdownDescription: "Flavor id, the value `flavor_id` takes."},
						"name":          schema.StringAttribute{Computed: true, MarkdownDescription: "Flavor name."},
						"vcpus":         schema.Int64Attribute{Computed: true, MarkdownDescription: "Number of vCPUs."},
						"ram_mb":        schema.Int64Attribute{Computed: true, MarkdownDescription: "Memory, in MB."},
						"disk_gb":       schema.Int64Attribute{Computed: true, MarkdownDescription: "Disk, in GB."},
						"service_level": schema.StringAttribute{Computed: true, MarkdownDescription: "Service level of instances of this flavor, such as `99.9%` or `best-effort`."},
					},
				},
			},
		},
	}
}

func (d *flavorsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromDataSourceProvider(req, resp)
}

func (d *flavorsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config flavorsModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	flavors, err := d.client.Compute.ListFlavors(ctx)
	if err != nil {
		detail := err.Error()
		if nemau.IsForbidden(err) {
			detail += "\n\nFlavors require the `compute-flavors` organization feature. " +
				"Without it, size instances with vcpus, ram_mb and disk_gb."
		}
		resp.Diagnostics.AddError("Cannot read the flavors", detail)
		return
	}
	config.Flavors = make([]flavorEntry, 0, len(flavors))
	for _, flavor := range flavors {
		if !config.NameFilter.IsNull() && flavor.Name != config.NameFilter.ValueString() {
			continue
		}
		config.Flavors = append(config.Flavors, flavorEntry{
			ID:           types.StringValue(flavor.ID),
			Name:         types.StringValue(flavor.Name),
			VCPUs:        types.Int64Value(int64(flavor.VCPUs)),
			RAMMB:        types.Int64Value(int64(flavor.RAMMB)),
			DiskGB:       types.Int64Value(int64(flavor.DiskGB)),
			ServiceLevel: str(flavor.ServiceLevel),
		})
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}

// ---------------------------------------------------------------------------
// Regions
// ---------------------------------------------------------------------------

var (
	_ datasource.DataSource              = (*regionsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*regionsDataSource)(nil)
)

func NewRegionsDataSource() datasource.DataSource { return &regionsDataSource{} }

type regionsDataSource struct{ client *nemau.Client }

type regionEntry struct {
	Slug        types.String `tfsdk:"slug"`
	DisplayName types.String `tfsdk:"display_name"`
	Country     types.String `tfsdk:"country"`
}

type regionsModel struct {
	Regions []regionEntry `tfsdk:"regions"`
}

func (d *regionsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_regions"
}

func (d *regionsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The regions that are enabled.\n\n" +
			"A project's region is chosen when it is created, in the console: no provider " +
			"resource takes it as an argument.",
		Attributes: map[string]schema.Attribute{
			"regions": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The enabled regions.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"slug":         schema.StringAttribute{Computed: true, MarkdownDescription: "Short identifier of the region."},
						"display_name": schema.StringAttribute{Computed: true, MarkdownDescription: "Human-readable name."},
						"country":      schema.StringAttribute{Computed: true, MarkdownDescription: "Country the region is in."},
					},
				},
			},
		},
	}
}

func (d *regionsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromDataSourceProvider(req, resp)
}

func (d *regionsDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	regions, err := d.client.Regions.List(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Cannot read the regions", err.Error())
		return
	}
	model := regionsModel{Regions: make([]regionEntry, 0, len(regions))}
	for _, region := range regions {
		model.Regions = append(model.Regions, regionEntry{
			Slug:        types.StringValue(region.Slug),
			DisplayName: str(region.DisplayName),
			Country:     str(region.Country),
		})
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}

// ---------------------------------------------------------------------------
// Networks
// ---------------------------------------------------------------------------

var (
	_ datasource.DataSource              = (*networksDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*networksDataSource)(nil)
)

func NewNetworksDataSource() datasource.DataSource { return &networksDataSource{} }

type networksDataSource struct{ client *nemau.Client }

type networkEntry struct {
	ID       types.String `tfsdk:"id"`
	Name     types.String `tfsdk:"name"`
	Status   types.String `tfsdk:"status"`
	External types.Bool   `tfsdk:"external"`
}

type networksModel struct {
	NameFilter types.String   `tfsdk:"name"`
	Networks   []networkEntry `tfsdk:"networks"`
}

func (d *networksDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_networks"
}

func (d *networksDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The project's networks, including the region's external network " +
			"(`external = true`), useful for allocating a floating IP explicitly.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Keep only the networks whose name matches this value exactly.",
			},
			"networks": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The matching networks.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id":       schema.StringAttribute{Computed: true, MarkdownDescription: "Network id."},
						"name":     schema.StringAttribute{Computed: true, MarkdownDescription: "Network name."},
						"status":   schema.StringAttribute{Computed: true, MarkdownDescription: "Status as reported by the platform."},
						"external": schema.BoolAttribute{Computed: true, MarkdownDescription: "True for the region's external network."},
					},
				},
			},
		},
	}
}

func (d *networksDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromDataSourceProvider(req, resp)
}

func (d *networksDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config networksModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	networks, err := d.client.Network.ListNetworks(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Cannot read the networks", err.Error())
		return
	}
	config.Networks = make([]networkEntry, 0, len(networks))
	for _, network := range networks {
		if !config.NameFilter.IsNull() && network.Name != config.NameFilter.ValueString() {
			continue
		}
		config.Networks = append(config.Networks, networkEntry{
			ID:       types.StringValue(network.ID),
			Name:     types.StringValue(network.Name),
			Status:   str(network.Status),
			External: types.BoolValue(network.External),
		})
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}

// ---------------------------------------------------------------------------
// Instance
// ---------------------------------------------------------------------------

var (
	_ datasource.DataSource              = (*instanceDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*instanceDataSource)(nil)
)

func NewInstanceDataSource() datasource.DataSource { return &instanceDataSource{} }

type instanceDataSource struct{ client *nemau.Client }

type instanceDataModel struct {
	ID        types.String `tfsdk:"id"`
	Name      types.String `tfsdk:"name"`
	Status    types.String `tfsdk:"status"`
	VCPUs     types.Int64  `tfsdk:"vcpus"`
	RAMMB     types.Int64  `tfsdk:"ram_mb"`
	DiskGB    types.Int64  `tfsdk:"disk_gb"`
	Addresses types.List   `tfsdk:"addresses"`
	KeyName   types.String `tfsdk:"key_name"`
}

func (d *instanceDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_instance"
}

func (d *instanceDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An existing instance, for referencing a machine Terraform does not manage.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Instance id.",
			},
			"name":    schema.StringAttribute{Computed: true, MarkdownDescription: "Instance name."},
			"status":  schema.StringAttribute{Computed: true, MarkdownDescription: "Reported status: running, stopped, paused or pending."},
			"vcpus":   schema.Int64Attribute{Computed: true, MarkdownDescription: "Number of vCPUs."},
			"ram_mb":  schema.Int64Attribute{Computed: true, MarkdownDescription: "Memory, in MB."},
			"disk_gb": schema.Int64Attribute{Computed: true, MarkdownDescription: "System disk, in GB."},
			"addresses": schema.ListAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Known IP addresses. Empty until the guest has reported its network configuration.",
			},
			"key_name": schema.StringAttribute{Computed: true, MarkdownDescription: "Name of the SSH keypair injected at boot."},
		},
	}
}

func (d *instanceDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromDataSourceProvider(req, resp)
}

func (d *instanceDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config instanceDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	instance, err := d.client.Compute.GetInstance(ctx, config.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Cannot read the instance", err.Error())
		return
	}
	config.Name = types.StringValue(instance.Name)
	config.Status = str(instance.Status)
	config.VCPUs = types.Int64Value(int64(instance.Sizing.VCPUs))
	config.RAMMB = types.Int64Value(int64(instance.Sizing.RAMMB))
	config.DiskGB = types.Int64Value(int64(instance.Sizing.DiskGB))
	config.Addresses = strList(instance.Addresses)
	config.KeyName = str(instance.KeyName)
	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}

// ---------------------------------------------------------------------------
// Kubernetes versions
// ---------------------------------------------------------------------------

var (
	_ datasource.DataSource              = (*kubernetesVersionsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*kubernetesVersionsDataSource)(nil)
)

func NewKubernetesVersionsDataSource() datasource.DataSource {
	return &kubernetesVersionsDataSource{}
}

type kubernetesVersionsDataSource struct{ client *nemau.Client }

type kubernetesVersionsModel struct {
	Versions types.List   `tfsdk:"versions"`
	Latest   types.String `tfsdk:"latest"`
}

func (d *kubernetesVersionsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_kubernetes_versions"
}

func (d *kubernetesVersionsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The supported Kubernetes versions.",
		Attributes: map[string]schema.Attribute{
			"versions": schema.ListAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "The versions a cluster can be created with or upgraded to.",
			},
			"latest": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The last entry in the list, in the platform's own order. " +
					"Pin an explicit version for a production cluster instead, because `latest` " +
					"changes when new versions are added.",
			},
		},
	}
}

func (d *kubernetesVersionsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromDataSourceProvider(req, resp)
}

func (d *kubernetesVersionsDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	versions, err := d.client.Kubernetes.ListVersions(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Cannot read the versions", err.Error())
		return
	}
	model := kubernetesVersionsModel{Versions: strList(versions), Latest: types.StringNull()}
	if len(versions) > 0 {
		model.Latest = types.StringValue(versions[len(versions)-1])
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}

// ---------------------------------------------------------------------------
// Kubeconfig
// ---------------------------------------------------------------------------

var (
	_ datasource.DataSource              = (*kubeconfigDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*kubeconfigDataSource)(nil)
)

func NewKubeconfigDataSource() datasource.DataSource { return &kubeconfigDataSource{} }

type kubeconfigDataSource struct{ client *nemau.Client }

type kubeconfigModel struct {
	ClusterName types.String `tfsdk:"cluster_name"`
	Raw         types.String `tfsdk:"raw"`
}

func (d *kubeconfigDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_kubeconfig"
}

func (d *kubeconfigDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A managed cluster's kubeconfig, for chaining into the Kubernetes " +
			"or Helm provider.\n\n" +
			"The result is stored in Terraform state in clear text and grants cluster-admin " +
			"rights; the platform audits each read (`GET_KUBECONFIG`). Encrypt your " +
			"state, or fetch the kubeconfig outside Terraform with " +
			"`nemau k8s cluster kubeconfig`.",
		Attributes: map[string]schema.Attribute{
			"cluster_name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Name of the cluster: `nemau_kubernetes_cluster.<name>.name`.",
			},
			"raw": schema.StringAttribute{
				Computed:            true,
				Sensitive:           true,
				MarkdownDescription: "The kubeconfig's YAML content.",
			},
		},
	}
}

func (d *kubeconfigDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromDataSourceProvider(req, resp)
}

func (d *kubeconfigDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config kubeconfigModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	raw, err := d.client.Kubernetes.GetKubeconfig(ctx, config.ClusterName.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Cannot read the kubeconfig", err.Error())
		return
	}
	config.Raw = types.StringValue(raw)
	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}
