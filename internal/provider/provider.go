// Package provider implements the Nemau Cloud Terraform provider.
//
// Writes are asynchronous. A create or delete answers 202 with a task id and
// the platform does the work in the background. Every Create and Delete
// submits the request and then waits for the task; the resource id comes from
// the completed task.
//
// Names, descriptions and other settings the API can change are updated in
// place through PATCH. Attributes that identify a resource carry
// RequiresReplace: a keypair's material, a container's bucket name, a record
// set's name and type, an instance's image and network. The plan then shows
// the replacement instead of attempting an update the API cannot make.
//
// The organization comes from the credential, and a resource cannot be moved
// out of it. The project comes from the provider's project argument when it is
// set, and from the credential otherwise, so two projects need two provider
// configurations but only one credential. A service account pinned to a
// project cannot use another one; the API answers 403.
package provider

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

// Ensure the implementation satisfies the framework interface.
var _ provider.Provider = (*nemauProvider)(nil)

type nemauProvider struct {
	version string
}

// New returns the provider factory the plugin server needs.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &nemauProvider{version: version}
	}
}

// providerModel mirrors the provider block.
type providerModel struct {
	APIURL          types.String `tfsdk:"api_url"`
	AccessKeyID     types.String `tfsdk:"access_key_id"`
	AccessKeySecret types.String `tfsdk:"access_key_secret"`
	Token           types.String `tfsdk:"token"`
	Project         types.String `tfsdk:"project"`
}

func (p *nemauProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "nemau"
	resp.Version = p.version
}

func (p *nemauProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The Nemau Cloud provider manages compute, network, load balancing, storage, " +
			"managed Kubernetes and DNS resources.\n\n" +
			"The organization always comes from the credential. The project comes from `project` " +
			"when it is set, and from the credential otherwise. To manage several projects with " +
			"one credential, declare one `alias` configuration per project, differing only in " +
			"`project`.",
		Attributes: map[string]schema.Attribute{
			"project": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Id of the project every resource in this configuration " +
					"acts on. Defaults to `NEMAU_PROJECT_ID`, then to the project the credential " +
					"carries. A service account pinned to a project cannot use another one: the " +
					"API answers 403.",
			},
			"api_url": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "API origin. Defaults to `NEMAU_API_URL`, then `" +
					nemau.DefaultBaseURL + "`.",
			},
			"access_key_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "IAM access key id. Defaults to `NEMAU_ACCESS_KEY_ID`. " +
					"A key expires 90 days after creation unless its owner chose another date " +
					"(365 days at most): rotate it before then.",
			},
			"access_key_secret": schema.StringAttribute{
				Optional:            true,
				Sensitive:           true,
				MarkdownDescription: "Access key secret. Defaults to `NEMAU_ACCESS_KEY_SECRET`.",
			},
			"token": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "Service-account token. Defaults to `NEMAU_TOKEN`. " +
					"It expires after 15 minutes, so prefer an access key for long runs.",
			},
		},
	}
}

func (p *nemauProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var config providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// An unknown value here means it comes from another resource's output and is
	// not resolved yet. Terraform cannot configure a provider from that, and the
	// message has to say which attribute so the user can hoist it into a
	// variable.
	for _, unknown := range []struct {
		attr  path.Path
		value types.String
		env   string
	}{
		{path.Root("api_url"), config.APIURL, nemau.EnvAPIURL},
		{path.Root("access_key_id"), config.AccessKeyID, nemau.EnvAccessKeyID},
		{path.Root("access_key_secret"), config.AccessKeySecret, nemau.EnvAccessKeySecret},
		{path.Root("token"), config.Token, nemau.EnvToken},
		{path.Root("project"), config.Project, nemau.EnvProject},
	} {
		if unknown.value.IsUnknown() {
			resp.Diagnostics.AddAttributeError(unknown.attr,
				"Unknown provider configuration",
				"The value is not known when the provider is configured. "+
					"Set it from a variable or from the environment variable "+unknown.env+".")
		}
	}
	if resp.Diagnostics.HasError() {
		return
	}

	options := []nemau.Option{nemau.WithUserAgent("terraform-provider-nemau/" + p.version)}

	if url := firstNonEmpty(config.APIURL.ValueString(), os.Getenv(nemau.EnvAPIURL)); url != "" {
		options = append(options, nemau.WithBaseURL(url))
	}

	// The value is a project id. Terraform resolves nothing here: a project
	// name sent as an id would match no project, and the error would only
	// appear on the first resource.
	if project := firstNonEmpty(config.Project.ValueString(), os.Getenv(nemau.EnvProject)); project != "" {
		options = append(options, nemau.WithProject(project))
	}

	keyID := firstNonEmpty(config.AccessKeyID.ValueString(), os.Getenv(nemau.EnvAccessKeyID))
	keySecret := firstNonEmpty(config.AccessKeySecret.ValueString(), os.Getenv(nemau.EnvAccessKeySecret))
	token := firstNonEmpty(config.Token.ValueString(), os.Getenv(nemau.EnvToken))

	switch {
	case keyID != "" && keySecret != "":
		options = append(options, nemau.WithCredential(nemau.AccessKey{ID: keyID, Secret: keySecret}))
	case token != "":
		options = append(options, nemau.WithCredential(nemau.BearerToken(token)))
	case keyID != "" || keySecret != "":
		// Only one half of an access key is set. Treat it as a configuration
		// error rather than falling back to anonymous access, and name the
		// missing half.
		missing := "access_key_secret"
		if keyID == "" {
			missing = "access_key_id"
		}
		resp.Diagnostics.AddError("Incomplete credential",
			"An access key needs both halves; "+missing+" is missing.")
		return
	default:
		resp.Diagnostics.AddError("No credential",
			"Set access_key_id and access_key_secret, or token, or the environment "+
				"variables NEMAU_ACCESS_KEY_ID and NEMAU_ACCESS_KEY_SECRET, or NEMAU_TOKEN.")
		return
	}

	client, err := nemau.NewClient(options...)
	if err != nil {
		resp.Diagnostics.AddError("Unusable Nemau client", err.Error())
		return
	}

	tflog.Debug(ctx, "nemau client configured", map[string]any{
		"api_url":    client.BaseURL(),
		"credential": client.DescribeCredential(),
		"project":    client.Project(),
	})

	resp.DataSourceData = client
	resp.ResourceData = client
}

func (p *nemauProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewInstanceResource,
		NewKeypairResource,
		NewSnapshotResource,
		NewNetworkResource,
		NewSubnetResource,
		NewFloatingIPResource,
		NewFloatingIPAssociationResource,
		NewSecurityGroupResource,
		NewSecurityGroupRuleResource,
		NewVolumeResource,
		NewVolumeAttachmentResource,
		NewContainerResource,
		NewContainerCredentialResource,
		NewLoadBalancerResource,
		NewListenerResource,
		NewLoadBalancerCertificateResource,
		NewPoolResource,
		NewPoolMemberResource,
		NewHealthMonitorResource,
		NewDNSZoneResource,
		NewDNSZoneVerificationResource,
		NewDNSRecordSetResource,
		NewKubernetesClusterResource,
		NewKubernetesNodePoolResource,
		NewDatabaseResource,
		NewInstanceGroupResource,
	}
}

func (p *nemauProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewImagesDataSource,
		NewFlavorsDataSource,
		NewRegionsDataSource,
		NewNetworksDataSource,
		NewInstanceDataSource,
		NewKubernetesVersionsDataSource,
		NewKubeconfigDataSource,
		NewQuotasDataSource,
		NewDatabaseDataSource,
		NewDatabaseFlavorsDataSource,
		NewDatabaseVersionsDataSource,
		NewInstanceGroupDataSource,
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
