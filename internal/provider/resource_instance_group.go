package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

// ---------------------------------------------------------------------------
// Instance group
// ---------------------------------------------------------------------------

var (
	_ resource.Resource                   = (*instanceGroupResource)(nil)
	_ resource.ResourceWithConfigure      = (*instanceGroupResource)(nil)
	_ resource.ResourceWithImportState    = (*instanceGroupResource)(nil)
	_ resource.ResourceWithValidateConfig = (*instanceGroupResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*instanceGroupResource)(nil)
)

func NewInstanceGroupResource() resource.Resource { return &instanceGroupResource{} }

type instanceGroupResource struct{ client *nemau.Client }

type instanceGroupModel struct {
	ID                      types.String `tfsdk:"id"`
	Name                    types.String `tfsdk:"name"`
	NetworkID               types.String `tfsdk:"network_id"`
	SubnetID                types.String `tfsdk:"subnet_id"`
	MinSize                 types.Int64  `tfsdk:"min_size"`
	MaxSize                 types.Int64  `tfsdk:"max_size"`
	DesiredCapacity         types.Int64  `tfsdk:"desired_capacity"`
	SecurityGroups          types.List   `tfsdk:"security_groups"`
	HealthCheck             types.String `tfsdk:"health_check"`
	HealthCheckGraceSeconds types.Int64  `tfsdk:"health_check_grace_seconds"`
	DrainSeconds            types.Int64  `tfsdk:"drain_seconds"`
	MaxUnavailablePct       types.Int64  `tfsdk:"max_unavailable_pct"`
	MaxSurge                types.Int64  `tfsdk:"max_surge"`
	LoadBalancerPools       types.List   `tfsdk:"load_balancer_pool"`
	ScalingPolicies         types.List   `tfsdk:"scaling_policy"`
	ScheduledActions        types.List   `tfsdk:"scheduled_action"`
	LaunchTemplate          types.Object `tfsdk:"launch_template"`
	DeleteInstancesOnDelete types.Bool   `tfsdk:"delete_instances_on_destroy"`
	RefreshOnTemplateChange types.Bool   `tfsdk:"refresh_on_template_change"`
	Status                  types.String `tfsdk:"status"`
	InstanceCount           types.Int64  `tfsdk:"instance_count"`
	TemplateVersion         types.Int64  `tfsdk:"template_version"`
}

type groupPoolModel struct {
	PoolID       types.String `tfsdk:"pool_id"`
	ProtocolPort types.Int64  `tfsdk:"protocol_port"`
}

type groupPolicyModel struct {
	Metric                  types.String `tfsdk:"metric"`
	Target                  types.Int64  `tfsdk:"target"`
	WindowSeconds           types.Int64  `tfsdk:"window_seconds"`
	ScaleOutCooldownSeconds types.Int64  `tfsdk:"scale_out_cooldown_seconds"`
	ScaleInCooldownSeconds  types.Int64  `tfsdk:"scale_in_cooldown_seconds"`
	ScaleInWindowSeconds    types.Int64  `tfsdk:"scale_in_window_seconds"`
	MaxStepOut              types.Int64  `tfsdk:"max_step_out"`
	MaxStepIn               types.Int64  `tfsdk:"max_step_in"`
	ScaleInEnabled          types.Bool   `tfsdk:"scale_in_enabled"`
}

type groupScheduleModel struct {
	Cron            cronValue   `tfsdk:"cron"`
	MinSize         types.Int64 `tfsdk:"min_size"`
	MaxSize         types.Int64 `tfsdk:"max_size"`
	DesiredCapacity types.Int64 `tfsdk:"desired_capacity"`
}

type launchTemplateModel struct {
	ImageID      types.String `tfsdk:"image_id"`
	FlavorID     types.String `tfsdk:"flavor_id"`
	VCPUs        types.Int64  `tfsdk:"vcpus"`
	RAMMB        types.Int64  `tfsdk:"ram_mb"`
	DiskGB       types.Int64  `tfsdk:"disk_gb"`
	KeyName      types.String `tfsdk:"key_name"`
	ServiceLevel types.String `tfsdk:"service_level"`
	UserData     types.String `tfsdk:"user_data"`
}

var (
	groupPoolAttrTypes   = map[string]attr.Type{"pool_id": types.StringType, "protocol_port": types.Int64Type}
	groupPolicyAttrTypes = map[string]attr.Type{
		"metric": types.StringType, "target": types.Int64Type, "window_seconds": types.Int64Type,
		"scale_out_cooldown_seconds": types.Int64Type, "scale_in_cooldown_seconds": types.Int64Type,
		"scale_in_window_seconds": types.Int64Type, "max_step_out": types.Int64Type,
		"max_step_in": types.Int64Type, "scale_in_enabled": types.BoolType,
	}
	groupScheduleAttrTypes = map[string]attr.Type{
		"cron": cronType{}, "min_size": types.Int64Type, "max_size": types.Int64Type,
		"desired_capacity": types.Int64Type,
	}
	launchTemplateAttrTypes = map[string]attr.Type{
		"image_id": types.StringType, "flavor_id": types.StringType, "vcpus": types.Int64Type,
		"ram_mb": types.Int64Type, "disk_gb": types.Int64Type, "key_name": types.StringType,
		"service_level": types.StringType, "user_data": types.StringType,
	}
)

// Limits of an instance group, as the platform checks them.
const (
	groupMaxMembers   = 50
	groupMaxUserData  = 16 * 1024
	groupMaxPools     = 4
	groupMaxPolicies  = 2
	groupMaxSchedules = 10
	groupMaxSecurity  = 5
)

var (
	groupNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	groupCronField   = regexp.MustCompile(`^[0-9A-Za-z*/,-]{1,32}$`)
)

func (r *instanceGroupResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_instance_group"
}

func (r *instanceGroupResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An instance group: between `min_size` and `max_size` identical instances, created from one " +
			"launch template, that scale with target-tracking policies and scheduled actions. The group " +
			"is free; its instances are billed as instances are. Growing a group needs a billable organization.\n\n" +
			"**The autoscaler owns `desired_capacity`.** It is Optional and Computed: leave it out and Terraform " +
			"never fights the autoscaler. Set it and Terraform sets the capacity once, at the next apply that " +
			"changes it. A scheduled action or a policy then moves it again.\n\n" +
			"**Replacement.** Changing `name`, `network_id` or `subnet_id` replaces the group. Deleting a group " +
			"that has members is refused unless `delete_instances_on_destroy` is true, which deletes them.\n\n" +
			"**user_data is never read back.** The platform seals the document and returns only whether one is " +
			"set, and a digest of it. An import leaves the document empty: set it again after an import. The provider compares the platform's SHA-256 " +
			"digest of the document with the configured one, so a document changed outside Terraform shows as a " +
			"change. A group cannot lose its user_data: the plan refuses the removal, and replacing the group is the " +
			"way to remove it.\n\n" +
			"A change of the launch template is a new template version. The members move to it when a rolling " +
			"refresh runs: the platform starts one for a template change when `refresh_on_template_change` is " +
			"true, with the group's stored `max_unavailable_pct` and `max_surge`. Without it, the new version is " +
			"stored and the members keep their version until a refresh runs. The apply waits for the refresh to " +
			"be accepted, not for every member to roll; the platform reports the refresh's state in the group's view.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The group's public id.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "A DNS label: lowercase letters, digits and hyphens, 56 characters at most. " +
					"Changing it replaces the group.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"network_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Network of the members. Changing it replaces the group.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"subnet_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Subnet of the members. Changing it replaces the group.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"min_size": schema.Int64Attribute{
				Required:            true,
				MarkdownDescription: "The autoscaler never goes below this many members, 0 to 50. Changes in place.",
			},
			"max_size": schema.Int64Attribute{
				Required:            true,
				MarkdownDescription: "The autoscaler never goes above this many members, 0 to 50. Changes in place.",
			},
			"desired_capacity": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Members the group should run now, between `min_size` and `max_size`. The autoscaler " +
					"moves it at runtime. When set, Terraform applies the value when the configuration changes it, " +
					"and does not read the autoscaler's moves back: the state keeps the value last applied. When " +
					"omitted, the autoscaler owns it and the state shows the platform's value.",
			},
			"security_groups": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				MarkdownDescription: "Up to 5 security group ids the members are filtered with. Changes in place; " +
					"removing them all clears the list.",
			},
			"health_check": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "How an unhealthy member is detected: `instance`, the only one for now. " +
					"Set by the platform when omitted.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"health_check_grace_seconds": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Seconds a new member is given before its health is checked, 60 to 1800. Set by the " +
					"platform when omitted.",
				PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"drain_seconds": schema.Int64Attribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Seconds a member drains before it is removed, 0 to 300. Set by the platform when omitted.",
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"max_unavailable_pct": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Percent of members a rolling update may take down at once, 0 to 50. Set by the " +
					"platform when omitted.",
				PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"max_surge": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Extra members a rolling update may start at once, 1 to 10. Set by the platform when " +
					"omitted.",
				PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"delete_instances_on_destroy": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Delete the members when the group is destroyed. Off by default: a group that still " +
					"has members is then refused at destroy.",
			},
			"status": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "State of the group as the platform reports it.",
			},
			"instance_count": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Current number of members.",
			},
			"template_version": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Version of the launch template the members run.",
			},
			"refresh_on_template_change": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Start a rolling refresh when the launch template changes, so that the members move " +
					"to the new version at once, within the group's `max_unavailable_pct` and `max_surge`. Off by default: " +
					"a template change is then stored, and the members move only when a refresh runs. Changing this flag " +
					"alone starts nothing.",
			},
		},
		Blocks: map[string]schema.Block{
			"launch_template": schema.SingleNestedBlock{
				MarkdownDescription: "What each member is created from (required). A change is a new template version.",
				Attributes: map[string]schema.Attribute{
					"image_id": schema.StringAttribute{
						Required:            true,
						MarkdownDescription: "Image id of the members.",
					},
					"flavor_id": schema.StringAttribute{
						Optional: true,
						Computed: true,
						MarkdownDescription: "Flavor id of the members. Exclusive with the custom size: " +
							"`vcpus`, `ram_mb` and `disk_gb` together.",
						PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
					},
					"vcpus": schema.Int64Attribute{
						Optional: true,
						Computed: true,
						MarkdownDescription: "vCPUs of the members, 1 to 64. Set with `ram_mb` and `disk_gb`, " +
							"instead of `flavor_id`.",
						PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
					},
					"ram_mb": schema.Int64Attribute{
						Optional:            true,
						Computed:            true,
						MarkdownDescription: "Memory of the members in MB: a multiple of 256, from 1024 to 262144.",
						PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
					},
					"disk_gb": schema.Int64Attribute{
						Optional:            true,
						Computed:            true,
						MarkdownDescription: "Disk of the members in GB, 10 to 5000.",
						PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
					},
					"key_name": schema.StringAttribute{
						Optional:            true,
						Computed:            true,
						MarkdownDescription: "SSH keypair of the members.",
						PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
					},
					"service_level": schema.StringAttribute{
						Optional:            true,
						Computed:            true,
						MarkdownDescription: "`best-effort` (the default) or `99.9%`. After importing a group, the first apply with `service_level` set re-sends the launch template once.",
						PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
					},
					"user_data": schema.StringAttribute{
						Optional:  true,
						Sensitive: true,
						MarkdownDescription: "cloud-init document of the members, at most 16 KiB, starting with `#cloud-config`, " +
							"`#!` or a MIME multipart header. It may hold secrets. The platform never returns it: the " +
							"provider compares digests. Cannot be removed from a group; replace the group instead.",
					},
				},
			},
			"load_balancer_pool": schema.ListNestedBlock{
				MarkdownDescription: "A load balancer pool the members register in. At most 4.",
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"pool_id": schema.StringAttribute{
							Required:            true,
							MarkdownDescription: "Id of the pool.",
						},
						"protocol_port": schema.Int64Attribute{
							Required:            true,
							MarkdownDescription: "Port of the members in the pool, 1 to 65535.",
						},
					},
				},
			},
			"scaling_policy": schema.ListNestedBlock{
				MarkdownDescription: "A target-tracking policy: the group adds or removes members to keep the metric near " +
					"the target. At most 2, one per metric. Only `cpu_percent` is available. Unset values take the " +
					"platform's defaults.",
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"metric": schema.StringAttribute{
							Required:            true,
							MarkdownDescription: "`cpu_percent`.",
						},
						"target": schema.Int64Attribute{
							Required:            true,
							MarkdownDescription: "Value the metric is kept near, in percent, 10 to 90.",
						},
						"window_seconds": schema.Int64Attribute{
							Optional: true, Computed: true,
							MarkdownDescription: "Measurement window in seconds, 180 to 900.",
						},
						"scale_out_cooldown_seconds": schema.Int64Attribute{
							Optional: true, Computed: true,
							MarkdownDescription: "Seconds between two scale-outs, 60 to 3600.",
						},
						"scale_in_cooldown_seconds": schema.Int64Attribute{
							Optional: true, Computed: true,
							MarkdownDescription: "Seconds between two scale-ins, 60 to 3600.",
						},
						"scale_in_window_seconds": schema.Int64Attribute{
							Optional: true, Computed: true,
							MarkdownDescription: "Seconds of low load before a scale-in, 300 to 3600.",
						},
						"max_step_out": schema.Int64Attribute{
							Optional: true, Computed: true,
							MarkdownDescription: "Members added at once, 1 to 10.",
						},
						"max_step_in": schema.Int64Attribute{
							Optional: true, Computed: true,
							MarkdownDescription: "Members removed at once, 1 to 5.",
						},
						"scale_in_enabled": schema.BoolAttribute{
							Optional: true, Computed: true,
							MarkdownDescription: "Whether the policy may remove members.",
						},
					},
				},
			},
			"scheduled_action": schema.ListNestedBlock{
				MarkdownDescription: "A scheduled action: at each UTC cron time, the group takes the bounds it names. " +
					"At most 10. Each action sets at least one of the three sizes.",
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"cron": schema.StringAttribute{
							CustomType:          cronType{},
							Required:            true,
							MarkdownDescription: "Five cron fields in UTC, for example `0 8 * * 1-5`.",
						},
						"min_size": schema.Int64Attribute{
							Optional:            true,
							MarkdownDescription: "Minimum members from this time, 0 to 50.",
						},
						"max_size": schema.Int64Attribute{
							Optional:            true,
							MarkdownDescription: "Maximum members from this time, 0 to 50.",
						},
						"desired_capacity": schema.Int64Attribute{
							Optional:            true,
							MarkdownDescription: "Desired members from this time, 0 to 50.",
						},
					},
				},
			},
		},
	}
}

func (r *instanceGroupResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromResourceProvider(req, resp)
}

func (r *instanceGroupResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	var out privateKeys
	if resp.Private != nil {
		out = resp.Private
	}
	importGroup(ctx, req, resp, out)
}

// importGroup is ImportState with the private state given apart, so that a test
// can supply one.
func importGroup(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse, out privateKeys) {
	importByID(ctx, req, resp)
	if out != nil && !resp.Diagnostics.HasError() {
		resp.Diagnostics.Append(out.SetKey(ctx, serviceLevelUnknownKey, []byte("true"))...)
	}
}

// ValidateConfig checks the rules that need no round trip. The platform
// judges the rest, against the stored group where a field is merged.
func (r *instanceGroupResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var c instanceGroupModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &c)...)
	if resp.Diagnostics.HasError() {
		return
	}
	known := func(v types.Int64) bool { return !v.IsUnknown() && !v.IsNull() }
	bad := func(field, summary, detail string) {
		resp.Diagnostics.AddAttributeError(path.Root(field), summary, detail)
	}

	if !c.Name.IsUnknown() && !groupNamePattern.MatchString(c.Name.ValueString()) {
		bad("name", "Invalid group name", "name must be 1 to 56 lowercase letters, digits or hyphens, starting and ending with a letter or digit.")
	} else if !c.Name.IsUnknown() && len(c.Name.ValueString()) > 56 {
		bad("name", "Invalid group name", "name must be at most 56 characters.")
	}
	for _, f := range []struct {
		field string
		v     types.Int64
	}{{"min_size", c.MinSize}, {"max_size", c.MaxSize}, {"desired_capacity", c.DesiredCapacity}} {
		if known(f.v) && (f.v.ValueInt64() < 0 || f.v.ValueInt64() > groupMaxMembers) {
			bad(f.field, "Size out of range", fmt.Sprintf("%s must be between 0 and %d.", f.field, groupMaxMembers))
		}
	}
	if known(c.MinSize) && known(c.MaxSize) && c.MinSize.ValueInt64() > c.MaxSize.ValueInt64() {
		bad("min_size", "Invalid bounds", "min_size must not exceed max_size.")
	}
	if known(c.MinSize) && known(c.MaxSize) && known(c.DesiredCapacity) {
		if d := c.DesiredCapacity.ValueInt64(); d < c.MinSize.ValueInt64() || d > c.MaxSize.ValueInt64() {
			bad("desired_capacity", "Desired capacity out of bounds", "desired_capacity must be between min_size and max_size.")
		}
	}
	if known(c.HealthCheckGraceSeconds) {
		if g := c.HealthCheckGraceSeconds.ValueInt64(); g < 60 || g > 1800 {
			bad("health_check_grace_seconds", "Grace out of range", "health_check_grace_seconds must be between 60 and 1800.")
		}
	}
	if known(c.DrainSeconds) {
		if d := c.DrainSeconds.ValueInt64(); d < 0 || d > 300 {
			bad("drain_seconds", "Drain out of range", "drain_seconds must be between 0 and 300.")
		}
	}
	if known(c.MaxUnavailablePct) {
		if v := c.MaxUnavailablePct.ValueInt64(); v < 0 || v > 50 {
			bad("max_unavailable_pct", "Out of range", "max_unavailable_pct must be between 0 and 50.")
		}
	}
	if known(c.MaxSurge) {
		if v := c.MaxSurge.ValueInt64(); v < 1 || v > 10 {
			bad("max_surge", "Out of range", "max_surge must be between 1 and 10.")
		}
	}
	if !c.HealthCheck.IsUnknown() && !c.HealthCheck.IsNull() {
		switch c.HealthCheck.ValueString() {
		case "instance":
		case "load_balancer":
			bad("health_check", "Health check not available", "load_balancer is not available yet; use instance.")
		default:
			bad("health_check", "Unknown health check", "health_check must be instance.")
		}
	}
	if !c.SecurityGroups.IsUnknown() && len(c.SecurityGroups.Elements()) > groupMaxSecurity {
		bad("security_groups", "Too many security groups", fmt.Sprintf("a group accepts at most %d security groups.", groupMaxSecurity))
	}
	validateGroupBlocks(ctx, c, resp)
	validateLaunchTemplateConfig(ctx, c.LaunchTemplate, resp)
	if c.LaunchTemplate.IsNull() {
		bad("launch_template", "launch_template is required", "Set the launch_template block: at least image_id.")
	}
}

// validateGroupBlocks checks the pools, policies and schedules.
func validateGroupBlocks(ctx context.Context, c instanceGroupModel, resp *resource.ValidateConfigResponse) {
	if !c.LoadBalancerPools.IsUnknown() {
		var pools []groupPoolModel
		resp.Diagnostics.Append(c.LoadBalancerPools.ElementsAs(ctx, &pools, false)...)
		if len(pools) > groupMaxPools {
			resp.Diagnostics.AddError("Too many load balancer pools", fmt.Sprintf("a group accepts at most %d pools.", groupMaxPools))
		}
		seen := map[string]bool{}
		for _, p := range pools {
			if p.PoolID.IsUnknown() || p.ProtocolPort.IsUnknown() {
				continue
			}
			if p.ProtocolPort.ValueInt64() < 1 || p.ProtocolPort.ValueInt64() > 65535 {
				resp.Diagnostics.AddError("Invalid pool port", "protocol_port must be between 1 and 65535.")
			}
			if seen[p.PoolID.ValueString()] {
				resp.Diagnostics.AddError("Pool listed twice", "a load balancer pool may appear once per group.")
			}
			seen[p.PoolID.ValueString()] = true
		}
	}
	if !c.ScalingPolicies.IsUnknown() {
		var policies []groupPolicyModel
		resp.Diagnostics.Append(c.ScalingPolicies.ElementsAs(ctx, &policies, false)...)
		if len(policies) > groupMaxPolicies {
			resp.Diagnostics.AddError("Too many scaling policies", fmt.Sprintf("a group accepts at most %d policies.", groupMaxPolicies))
		}
		metrics := map[string]bool{}
		for _, p := range policies {
			if p.Metric.IsUnknown() || p.Target.IsUnknown() {
				continue
			}
			if p.Metric.ValueString() != "cpu_percent" {
				resp.Diagnostics.AddError("Unknown scaling metric", "metric must be cpu_percent: the memory metric is not available yet.")
			}
			if metrics[p.Metric.ValueString()] {
				resp.Diagnostics.AddError("Metric listed twice", "a scaling policy metric may appear once per group.")
			}
			metrics[p.Metric.ValueString()] = true
			if t := p.Target.ValueInt64(); t < 10 || t > 90 {
				resp.Diagnostics.AddError("Target out of range", "target must be between 10 and 90.")
			}
		}
	}
	if !c.ScheduledActions.IsUnknown() {
		var actions []groupScheduleModel
		resp.Diagnostics.Append(c.ScheduledActions.ElementsAs(ctx, &actions, false)...)
		if len(actions) > groupMaxSchedules {
			resp.Diagnostics.AddError("Too many scheduled actions", fmt.Sprintf("a group accepts at most %d scheduled actions.", groupMaxSchedules))
		}
		for _, a := range actions {
			if a.Cron.IsUnknown() {
				continue
			}
			if fields := strings.Fields(a.Cron.ValueString()); len(fields) != 5 {
				resp.Diagnostics.AddError("Invalid cron", "a scheduled action's cron takes five fields, in UTC.")
			} else {
				for _, f := range fields {
					if !groupCronField.MatchString(f) {
						resp.Diagnostics.AddError("Invalid cron", "a scheduled action's cron takes five fields, in UTC.")
						break
					}
				}
			}
			if a.MinSize.IsNull() && a.MaxSize.IsNull() && a.DesiredCapacity.IsNull() {
				resp.Diagnostics.AddError("Empty scheduled action", "a scheduled action sets at least one of min_size, max_size, desired_capacity.")
			}
		}
	}
}

// validateLaunchTemplateConfig checks the template's sizing and user_data.
func validateLaunchTemplateConfig(ctx context.Context, obj types.Object, resp *resource.ValidateConfigResponse) {
	if obj.IsNull() || obj.IsUnknown() {
		return
	}
	var t launchTemplateModel
	resp.Diagnostics.Append(obj.As(ctx, &t, basetypes.ObjectAsOptions{})...)
	if resp.Diagnostics.HasError() {
		return
	}
	if t.FlavorID.IsNull() || t.FlavorID.IsUnknown() {
		sizing := !t.VCPUs.IsNull() || !t.RAMMB.IsNull() || !t.DiskGB.IsNull()
		if sizing && (t.VCPUs.IsNull() || t.RAMMB.IsNull() || t.DiskGB.IsNull()) {
			resp.Diagnostics.AddAttributeError(path.Root("launch_template"), "Incomplete sizing",
				"set flavor_id, or vcpus, ram_mb and disk_gb together.")
		}
	}
	if !t.VCPUs.IsUnknown() && !t.VCPUs.IsNull() && (t.VCPUs.ValueInt64() < 1 || t.VCPUs.ValueInt64() > 64) {
		resp.Diagnostics.AddAttributeError(path.Root("launch_template"), "vcpus out of range", "vcpus must be between 1 and 64.")
	}
	if !t.RAMMB.IsUnknown() && !t.RAMMB.IsNull() {
		if v := t.RAMMB.ValueInt64(); v < 1024 || v > 262144 || v%256 != 0 {
			resp.Diagnostics.AddAttributeError(path.Root("launch_template"), "ram_mb out of range",
				"ram_mb must be a multiple of 256 between 1024 and 262144.")
		}
	}
	if !t.DiskGB.IsUnknown() && !t.DiskGB.IsNull() && (t.DiskGB.ValueInt64() < 10 || t.DiskGB.ValueInt64() > 5000) {
		resp.Diagnostics.AddAttributeError(path.Root("launch_template"), "disk_gb out of range", "disk_gb must be between 10 and 5000.")
	}
	if !t.UserData.IsUnknown() && !t.UserData.IsNull() {
		// The message names the rule only: the document itself is never echoed.
		if err := checkUserDataDocument(t.UserData.ValueString()); err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("launch_template").AtName("user_data"), "Invalid user_data", err.Error())
		}
	}
}

// checkUserDataDocument applies the platform's rules to a cloud-init document.
func checkUserDataDocument(document string) error {
	document = strings.TrimPrefix(document, "\uFEFF")
	switch {
	case len(document) > groupMaxUserData:
		return fmt.Errorf("user_data must not exceed %d bytes", groupMaxUserData)
	case !utf8.ValidString(document):
		return errors.New("user_data must be valid UTF-8")
	case strings.IndexByte(document, 0) >= 0:
		return errors.New("user_data must not contain NUL bytes")
	case strings.TrimSpace(document) == "":
		return errors.New("user_data must not be empty")
	}
	first, _, _ := strings.Cut(document, "\n")
	first = strings.TrimRight(first, " \t\r")
	switch {
	case first == "#cloud-config", strings.HasPrefix(first, "#!"):
		return nil
	case strings.HasPrefix(strings.ToLower(first), "content-type: multipart/"):
		return nil
	}
	return errors.New("user_data must start with #cloud-config, #! or a MIME multipart header")
}

// desiredConfigKey is the private state key that holds the desired_capacity the
// configuration set at the last apply. It is empty when the configuration left
// the value unset.
const desiredConfigKey = "desired_capacity_config"

// serviceLevelUnknownKey is the private state key an import sets. The view does
// not return the service level, so an imported group's state holds none, and a
// configured one is not compared with it until an update has set it.
const serviceLevelUnknownKey = "service_level_unknown"

// privateKeys is the part of the framework's private state a group reads and
// writes. The framework's ProviderData satisfies it; a nil value reads empty.
type privateKeys interface {
	GetKey(ctx context.Context, key string) ([]byte, diag.Diagnostics)
	SetKey(ctx context.Context, key string, value []byte) diag.Diagnostics
}

// storedDesiredConfig reads the desired_capacity the configuration set at the
// last apply, as text. Empty means none was set, or none is known.
func storedDesiredConfig(ctx context.Context, private privateKeys) (string, diag.Diagnostics) {
	if private == nil {
		return "", nil
	}
	value, diags := private.GetKey(ctx, desiredConfigKey)
	return string(value), diags
}

// desiredCapacityAction decides whether an apply sends the desired capacity.
// It compares the configured value with the one the last apply set, never with
// the platform's value: the autoscaler moves the platform's value at runtime,
// and a refresh must not undo that. It returns the text to store for the next
// apply, which is empty when the configuration leaves the value unset.
func desiredCapacityAction(prior string, cfg types.Int64) (send bool, now string) {
	if !cfg.IsUnknown() && !cfg.IsNull() {
		now = strconv.FormatInt(cfg.ValueInt64(), 10)
	}
	return now != "" && now != prior, now
}

// serviceLevelUnknown reports whether the import marker is set.
func serviceLevelUnknown(ctx context.Context, private privateKeys) (bool, diag.Diagnostics) {
	if private == nil {
		return false, nil
	}
	value, diags := private.GetKey(ctx, serviceLevelUnknownKey)
	return string(value) == "true", diags
}

// knownStr, knownInt and knownBool read a value the configuration or the plan
// settled. Unknown and null read as the zero value, which the SDK omits.
func knownStr(v types.String) string {
	if v.IsUnknown() || v.IsNull() {
		return ""
	}
	return v.ValueString()
}

func knownInt(v types.Int64) int {
	if v.IsUnknown() || v.IsNull() {
		return 0
	}
	return int(v.ValueInt64())
}

// knownIntPtr is nil for an unknown or null value, so that the SDK sends the
// field only when it is set.
func knownIntPtr(v types.Int64) *int {
	if v.IsUnknown() || v.IsNull() {
		return nil
	}
	n := int(v.ValueInt64())
	return &n
}

// intOr takes the planned value when it is known, else the prior value of the
// same element, else zero: an unknown planned value keeps what the group has.
func intOr(v, prior types.Int64) int {
	if !v.IsUnknown() && !v.IsNull() {
		return int(v.ValueInt64())
	}
	return knownInt(prior)
}

// templateFromConfig reads the launch_template block of the configuration into
// the SDK value. Only the fields the configuration sets are filled: the others
// keep what the group already has.
func templateFromConfig(ctx context.Context, obj types.Object) (*nemau.LaunchTemplate, diag.Diagnostics) {
	var diags diag.Diagnostics
	if obj.IsNull() || obj.IsUnknown() {
		return nil, diags
	}
	var t launchTemplateModel
	diags.Append(obj.As(ctx, &t, basetypes.ObjectAsOptions{})...)
	if diags.HasError() {
		return nil, diags
	}
	return &nemau.LaunchTemplate{
		ImageID: knownStr(t.ImageID), FlavorID: knownStr(t.FlavorID),
		VCPUs: knownInt(t.VCPUs), RAMMB: knownInt(t.RAMMB), DiskGB: knownInt(t.DiskGB),
		KeyName: knownStr(t.KeyName), ServiceLevel: knownStr(t.ServiceLevel),
		UserData: knownStr(t.UserData),
	}, diags
}

// templateChanged reports whether the configured template differs from the
// stored one, the document included. The stored document is never returned,
// so the state's copy stands for it: Update compares by digest instead, see
// userDataDiffers.
func templateChanged(ctx context.Context, cfg types.Object, state types.Object) (bool, diag.Diagnostics) {
	fields, diags := templateFieldsChanged(ctx, cfg, state, false)
	if fields || diags.HasError() || cfg.IsNull() || cfg.IsUnknown() || state.IsNull() {
		return fields, diags
	}
	var c, s launchTemplateModel
	diags.Append(cfg.As(ctx, &c, basetypes.ObjectAsOptions{})...)
	diags.Append(state.As(ctx, &s, basetypes.ObjectAsOptions{})...)
	if diags.HasError() {
		return false, diags
	}
	if isSetString(c.UserData) || isSetString(s.UserData) {
		return c.UserData.ValueString() != s.UserData.ValueString(), diags
	}
	return false, diags
}

// templateFieldsChanged is templateChanged without the document. Only the
// fields the configuration sets are compared. unknownServiceLevel is set while
// the import marker is: the view does not return the service level, so the state
// of an imported group holds none. A configured service level is then sent once,
// since the group's actual level is not known.
func templateFieldsChanged(ctx context.Context, cfg types.Object, state types.Object, unknownServiceLevel bool) (bool, diag.Diagnostics) {
	var diags diag.Diagnostics
	if cfg.IsNull() || cfg.IsUnknown() || state.IsNull() {
		return false, diags
	}
	var c, s launchTemplateModel
	diags.Append(cfg.As(ctx, &c, basetypes.ObjectAsOptions{})...)
	diags.Append(state.As(ctx, &s, basetypes.ObjectAsOptions{})...)
	if diags.HasError() {
		return false, diags
	}
	setInt := func(v types.Int64) bool { return !v.IsUnknown() && !v.IsNull() }
	serviceLevelChanged := isSetString(c.ServiceLevel) &&
		(unknownServiceLevel || c.ServiceLevel.ValueString() != stateServiceLevel(s.ServiceLevel))
	changed := (isSetString(c.ImageID) && c.ImageID.ValueString() != s.ImageID.ValueString()) ||
		(isSetString(c.FlavorID) && c.FlavorID.ValueString() != s.FlavorID.ValueString()) ||
		(isSetString(c.KeyName) && c.KeyName.ValueString() != s.KeyName.ValueString()) ||
		serviceLevelChanged ||
		(setInt(c.VCPUs) && c.VCPUs.ValueInt64() != s.VCPUs.ValueInt64()) ||
		(setInt(c.RAMMB) && c.RAMMB.ValueInt64() != s.RAMMB.ValueInt64()) ||
		(setInt(c.DiskGB) && c.DiskGB.ValueInt64() != s.DiskGB.ValueInt64())
	return changed, diags
}

// isSetString reports whether a string is known and not null.
// knownBool reads a bool that is known: an unknown or null value is false.
func knownBool(v types.Bool) bool { return !v.IsUnknown() && !v.IsNull() && v.ValueBool() }

func isSetString(v types.String) bool { return !v.IsUnknown() && !v.IsNull() }

// defaultServiceLevel is the service level of a group whose configuration sets none.
const defaultServiceLevel = "best-effort"

// stateServiceLevel reads the service level the state holds. A group created
// without the attribute stores null, which is the default.
func stateServiceLevel(v types.String) string {
	if !isSetString(v) {
		return defaultServiceLevel
	}
	return v.ValueString()
}

// knownOrNull returns the value when it is set, and null otherwise.
func knownOrNull(v types.String) types.String {
	if isSetString(v) {
		return v
	}
	return types.StringNull()
}

// userDataDigest is the digest the platform keeps for a document: the lowercase
// hex SHA-256 of its bytes once a leading byte order mark is stripped, as the
// platform normalizes the document before hashing it. It identifies the document
// without showing it.
func userDataDigest(document string) string {
	sum := sha256.Sum256([]byte(strings.TrimPrefix(document, "\uFEFF")))
	return hex.EncodeToString(sum[:])
}

// reconcileUserData compares the document the state holds with the digest the
// platform reports. A document that differs is dropped from the state: the next
// plan then shows the configured document as a change, and the update rolls the
// template. A document that matches is kept as configured.
func reconcileUserData(ctx context.Context, m *instanceGroupModel, digest string) diag.Diagnostics {
	var diags diag.Diagnostics
	if m.LaunchTemplate.IsNull() || m.LaunchTemplate.IsUnknown() {
		return diags
	}
	var t launchTemplateModel
	diags.Append(m.LaunchTemplate.As(ctx, &t, basetypes.ObjectAsOptions{})...)
	if diags.HasError() || !isSetString(t.UserData) || userDataDigest(t.UserData.ValueString()) == digest {
		return diags
	}
	t.UserData = types.StringNull()
	obj, d := types.ObjectValueFrom(ctx, launchTemplateAttrTypes, t)
	diags.Append(d...)
	m.LaunchTemplate = obj
	return diags
}

// configuredUserData reads the document of the configuration. The second
// result is false when no document is configured.
func configuredUserData(ctx context.Context, obj types.Object) (string, bool, diag.Diagnostics) {
	var diags diag.Diagnostics
	if obj.IsNull() || obj.IsUnknown() {
		return "", false, diags
	}
	var t launchTemplateModel
	diags.Append(obj.As(ctx, &t, basetypes.ObjectAsOptions{})...)
	if diags.HasError() || !isSetString(t.UserData) {
		return "", false, diags
	}
	return t.UserData.ValueString(), true, diags
}

// poolsFromList reads the load_balancer_pool blocks.
func poolsFromList(ctx context.Context, list types.List, diags *diag.Diagnostics) []nemau.GroupPool {
	if list.IsNull() || list.IsUnknown() {
		return nil
	}
	var items []groupPoolModel
	diags.Append(list.ElementsAs(ctx, &items, false)...)
	out := make([]nemau.GroupPool, 0, len(items))
	for _, p := range items {
		out = append(out, nemau.GroupPool{PoolID: knownStr(p.PoolID), ProtocolPort: knownInt(p.ProtocolPort)})
	}
	return out
}

// policiesFromList reads the scaling_policy blocks. An unknown planned value
// takes the value of the same policy in the prior state, so that an unchanged
// tuning is never reset to the platform's defaults by an unrelated change.
func policiesFromList(ctx context.Context, list, prior types.List, diags *diag.Diagnostics) []nemau.ScalingPolicy {
	if list.IsNull() || list.IsUnknown() {
		return nil
	}
	var items []groupPolicyModel
	diags.Append(list.ElementsAs(ctx, &items, false)...)
	var priors []groupPolicyModel
	if !prior.IsNull() && !prior.IsUnknown() {
		diags.Append(prior.ElementsAs(ctx, &priors, false)...)
	}
	out := make([]nemau.ScalingPolicy, 0, len(items))
	for i, p := range items {
		var pr groupPolicyModel
		if i < len(priors) && priors[i].Metric.ValueString() == p.Metric.ValueString() {
			pr = priors[i]
		}
		policy := nemau.ScalingPolicy{
			Metric:                  knownStr(p.Metric),
			Target:                  knownInt(p.Target),
			WindowSeconds:           intOr(p.WindowSeconds, pr.WindowSeconds),
			ScaleOutCooldownSeconds: intOr(p.ScaleOutCooldownSeconds, pr.ScaleOutCooldownSeconds),
			ScaleInCooldownSeconds:  intOr(p.ScaleInCooldownSeconds, pr.ScaleInCooldownSeconds),
			ScaleInWindowSeconds:    intOr(p.ScaleInWindowSeconds, pr.ScaleInWindowSeconds),
			MaxStepOut:              intOr(p.MaxStepOut, pr.MaxStepOut),
			MaxStepIn:               intOr(p.MaxStepIn, pr.MaxStepIn),
		}
		switch {
		case !p.ScaleInEnabled.IsUnknown() && !p.ScaleInEnabled.IsNull():
			v := p.ScaleInEnabled.ValueBool()
			policy.ScaleInEnabled = &v
		case !pr.ScaleInEnabled.IsUnknown() && !pr.ScaleInEnabled.IsNull():
			v := pr.ScaleInEnabled.ValueBool()
			policy.ScaleInEnabled = &v
		}
		out = append(out, policy)
	}
	return out
}

// normalizeCron collapses the runs of whitespace of a cron expression and trims
// it, so that two spellings of one schedule compare equal.
func normalizeCron(spec string) string {
	return strings.Join(strings.Fields(spec), " ")
}

// schedulesFromList reads the scheduled_action blocks. The cron is sent
// normalized.
func schedulesFromList(ctx context.Context, list types.List, diags *diag.Diagnostics) []nemau.Schedule {
	if list.IsNull() || list.IsUnknown() {
		return nil
	}
	var items []groupScheduleModel
	diags.Append(list.ElementsAs(ctx, &items, false)...)
	out := make([]nemau.Schedule, 0, len(items))
	for _, a := range items {
		out = append(out, nemau.Schedule{
			Cron: normalizeCron(knownStr(a.Cron.StringValue)), MinSize: knownIntPtr(a.MinSize),
			MaxSize: knownIntPtr(a.MaxSize), DesiredCapacity: knownIntPtr(a.DesiredCapacity),
		})
	}
	return out
}

// listFromPools, listFromPolicies and listFromSchedules build the block lists
// from the API's values. An empty answer is an empty list, which is what an
// absent block reads as.
func listFromPools(ctx context.Context, pools []nemau.GroupPool, diags *diag.Diagnostics) types.List {
	items := make([]groupPoolModel, 0, len(pools))
	for _, p := range pools {
		items = append(items, groupPoolModel{types.StringValue(p.PoolID), types.Int64Value(int64(p.ProtocolPort))})
	}
	list, d := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: groupPoolAttrTypes}, items)
	diags.Append(d...)
	return list
}

func listFromPolicies(ctx context.Context, policies []nemau.ScalingPolicy, diags *diag.Diagnostics) types.List {
	items := make([]groupPolicyModel, 0, len(policies))
	for _, p := range policies {
		enabled := types.BoolNull()
		if p.ScaleInEnabled != nil {
			enabled = types.BoolValue(*p.ScaleInEnabled)
		}
		items = append(items, groupPolicyModel{
			Metric: types.StringValue(p.Metric), Target: types.Int64Value(int64(p.Target)),
			WindowSeconds:           types.Int64Value(int64(p.WindowSeconds)),
			ScaleOutCooldownSeconds: types.Int64Value(int64(p.ScaleOutCooldownSeconds)),
			ScaleInCooldownSeconds:  types.Int64Value(int64(p.ScaleInCooldownSeconds)),
			ScaleInWindowSeconds:    types.Int64Value(int64(p.ScaleInWindowSeconds)),
			MaxStepOut:              types.Int64Value(int64(p.MaxStepOut)),
			MaxStepIn:               types.Int64Value(int64(p.MaxStepIn)),
			ScaleInEnabled:          enabled,
		})
	}
	list, d := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: groupPolicyAttrTypes}, items)
	diags.Append(d...)
	return list
}

// listFromSchedules builds the scheduled_action blocks from the API's values.
// The API returns the cron normalized; a prior cron that normalizes to the same
// value is kept as written, so that the spelling in the configuration does not
// show as a change.
func listFromSchedules(ctx context.Context, schedules []nemau.Schedule, prior types.List, diags *diag.Diagnostics) types.List {
	var priors []groupScheduleModel
	if !prior.IsNull() && !prior.IsUnknown() {
		diags.Append(prior.ElementsAs(ctx, &priors, false)...)
	}
	items := make([]groupScheduleModel, 0, len(schedules))
	for i, a := range schedules {
		cron := cronValue{StringValue: types.StringValue(a.Cron)}
		if i < len(priors) && !priors[i].Cron.IsUnknown() && !priors[i].Cron.IsNull() &&
			normalizeCron(priors[i].Cron.ValueString()) == a.Cron {
			cron = priors[i].Cron
		}
		items = append(items, groupScheduleModel{
			Cron: cron, MinSize: optionalInt(a.MinSize),
			MaxSize: optionalInt(a.MaxSize), DesiredCapacity: optionalInt(a.DesiredCapacity),
		})
	}
	list, d := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: groupScheduleAttrTypes}, items)
	diags.Append(d...)
	return list
}

func optionalInt(v *int) types.Int64 {
	if v == nil {
		return types.Int64Null()
	}
	return types.Int64Value(int64(*v))
}

// fillGroup writes what the platform reports into the model. The user_data is
// kept: the platform does not return it. keepDesired keeps the model's
// desired_capacity when the configuration sets it: the value is then the one
// applied, not the autoscaler's current one.
func fillGroup(ctx context.Context, m *instanceGroupModel, g *nemau.InstanceGroup, keepDesired bool) diag.Diagnostics {
	var diags diag.Diagnostics
	m.Name = types.StringValue(g.Name)
	m.MinSize = types.Int64Value(int64(g.MinSize))
	m.MaxSize = types.Int64Value(int64(g.MaxSize))
	if !keepDesired || m.DesiredCapacity.IsUnknown() || m.DesiredCapacity.IsNull() {
		m.DesiredCapacity = types.Int64Value(int64(g.DesiredCapacity))
	}
	// The flag is config-only: the platform does not return it. An import leaves
	// it null, which reads as the default.
	if m.RefreshOnTemplateChange.IsNull() || m.RefreshOnTemplateChange.IsUnknown() {
		m.RefreshOnTemplateChange = types.BoolValue(false)
	}
	m.NetworkID = types.StringValue(g.LaunchTemplate.NetworkID)
	m.SubnetID = types.StringValue(g.LaunchTemplate.SubnetID)
	m.SecurityGroups = optionalStrList(g.LaunchTemplate.SecurityGroupIDs, m.SecurityGroups)
	m.HealthCheck = types.StringValue(g.HealthCheck)
	m.HealthCheckGraceSeconds = types.Int64Value(int64(g.HealthCheckGraceSeconds))
	m.DrainSeconds = types.Int64Value(int64(g.DrainSeconds))
	m.MaxUnavailablePct = types.Int64Value(int64(g.MaxUnavailablePct))
	m.MaxSurge = types.Int64Value(int64(g.MaxSurge))
	m.LoadBalancerPools = listFromPools(ctx, g.Pools, &diags)
	m.ScalingPolicies = listFromPolicies(ctx, g.ScalingPolicies, &diags)
	m.ScheduledActions = listFromSchedules(ctx, g.Schedules, m.ScheduledActions, &diags)
	m.LaunchTemplate = launchTemplateObject(ctx, g.LaunchTemplate, m.LaunchTemplate, &diags)
	m.Status = types.StringValue(g.Status)
	m.InstanceCount = types.Int64Value(int64(g.InstanceCount))
	m.TemplateVersion = types.Int64Value(int64(g.TemplateVersion))
	return diags
}

// launchTemplateObject builds the launch_template block from the API's view,
// keeping the user_data and the service level the prior value holds. The view
// carries neither. A prior value that is still unknown, as on a create, reads
// as null: state never holds an unknown after apply.
func launchTemplateObject(ctx context.Context, t nemau.LaunchTemplateInfo, prior types.Object, diags *diag.Diagnostics) types.Object {
	userData, serviceLevel := types.StringNull(), types.StringNull()
	if !prior.IsNull() && !prior.IsUnknown() {
		var p launchTemplateModel
		diags.Append(prior.As(ctx, &p, basetypes.ObjectAsOptions{})...)
		userData, serviceLevel = knownOrNull(p.UserData), knownOrNull(p.ServiceLevel)
	}
	obj, d := types.ObjectValueFrom(ctx, launchTemplateAttrTypes, launchTemplateModel{
		ImageID: types.StringValue(t.ImageID), FlavorID: str(t.FlavorID),
		VCPUs: types.Int64Value(int64(t.VCPUs)), RAMMB: types.Int64Value(int64(t.RAMMB)),
		DiskGB: types.Int64Value(int64(t.DiskGB)), KeyName: str(t.KeyName),
		ServiceLevel: serviceLevel, UserData: userData,
	})
	diags.Append(d...)
	return obj
}

func (r *instanceGroupResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var out privateKeys
	if resp.Private != nil {
		out = resp.Private
	}
	r.create(ctx, req, resp, out)
}

// create is Create with the private state given apart: out is written.
func (r *instanceGroupResource) create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse, out privateKeys) {
	var plan, cfg instanceGroupModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	tmpl, d := templateFromConfig(ctx, cfg.LaunchTemplate)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() || tmpl == nil {
		resp.Diagnostics.AddError("launch_template is required", "Set the launch_template block: at least image_id.")
		return
	}

	create := nemau.CreateInstanceGroupRequest{
		Name: knownStr(plan.Name), NetworkID: knownStr(plan.NetworkID), SubnetID: knownStr(plan.SubnetID),
		MinSize: knownInt(plan.MinSize), MaxSize: knownInt(plan.MaxSize),
		DesiredCapacity:         knownIntPtr(plan.DesiredCapacity),
		SecurityGroups:          stringsFromList(ctx, plan.SecurityGroups, &resp.Diagnostics),
		LaunchTemplate:          tmpl,
		LoadBalancerPools:       poolsFromList(ctx, plan.LoadBalancerPools, &resp.Diagnostics),
		HealthCheck:             knownStr(plan.HealthCheck),
		HealthCheckGraceSeconds: knownIntPtr(plan.HealthCheckGraceSeconds),
		DrainSeconds:            knownIntPtr(plan.DrainSeconds),
		ScalingPolicies:         policiesFromList(ctx, plan.ScalingPolicies, types.ListNull(types.ObjectType{AttrTypes: groupPolicyAttrTypes}), &resp.Diagnostics),
	}
	if resp.Diagnostics.HasError() {
		return
	}
	id := createAndWait(ctx, r.client, &resp.Diagnostics, "instance group", func() (*nemau.TaskRef, error) {
		return r.client.InstanceGroups.Create(ctx, create)
	}, "instance_group_id")
	if resp.Diagnostics.HasError() {
		return
	}
	g, err := r.client.InstanceGroups.Get(ctx, id)
	if err != nil {
		resp.Diagnostics.AddError("Reading the new instance group failed", err.Error())
		return
	}
	plan.ID = types.StringValue(id)
	// The create applied the configured desired capacity: that value stands.
	_, now := desiredCapacityAction("", cfg.DesiredCapacity)
	if out != nil {
		resp.Diagnostics.Append(out.SetKey(ctx, desiredConfigKey, []byte(now))...)
	}
	resp.Diagnostics.Append(fillGroup(ctx, &plan, g, now != "")...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *instanceGroupResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var private privateKeys
	if req.Private != nil {
		private = req.Private
	}
	r.read(ctx, req, resp, private)
}

// read is Read with the private state given apart, so that a test can supply
// one: the framework's ProviderData cannot be built outside the framework.
func (r *instanceGroupResource) read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse, private privateKeys) {
	var state instanceGroupModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	g, err := r.client.InstanceGroups.Get(ctx, state.ID.ValueString())
	if handleReadError(err, "instance group", resp) {
		return
	}
	// A configured desired capacity is not read back: the autoscaler moves it.
	prior, d := storedDesiredConfig(ctx, private)
	resp.Diagnostics.Append(d...)
	resp.Diagnostics.Append(fillGroup(ctx, &state, g, prior != "")...)
	resp.Diagnostics.Append(reconcileUserData(ctx, &state, g.LaunchTemplate.UserDataSHA256)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update applies a change. The private state is read from the plan's, which
// holds the prior state's value unless ModifyPlan changed it, and written to the
// response's.
func (r *instanceGroupResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var prior, out privateKeys
	if req.Private != nil {
		prior = req.Private
	}
	if resp.Private != nil {
		out = resp.Private
	}
	r.update(ctx, req, resp, prior, out)
}

// update is Update with the private state given apart: prior is read, out is written.
func (r *instanceGroupResource) update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse, prior, out privateKeys) {
	var plan, state, cfg instanceGroupModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()
	priorDesired, d := storedDesiredConfig(ctx, prior)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Each block is read twice, from the plan and from the state, so that only
	// what differs is sent: a list the API replaces is never resent unchanged.
	policies := nonNil(policiesFromList(ctx, plan.ScalingPolicies, state.ScalingPolicies, &resp.Diagnostics))
	statePolicies := nonNil(policiesFromList(ctx, state.ScalingPolicies, state.ScalingPolicies, &resp.Diagnostics))
	pools := nonNil(poolsFromList(ctx, plan.LoadBalancerPools, &resp.Diagnostics))
	statePools := nonNil(poolsFromList(ctx, state.LoadBalancerPools, &resp.Diagnostics))
	schedules := nonNil(schedulesFromList(ctx, plan.ScheduledActions, &resp.Diagnostics))
	stateSchedules := nonNil(schedulesFromList(ctx, state.ScheduledActions, &resp.Diagnostics))
	securityGroups := nonNil(stringsFromList(ctx, plan.SecurityGroups, &resp.Diagnostics))
	stateSecurityGroups := nonNil(stringsFromList(ctx, state.SecurityGroups, &resp.Diagnostics))
	if resp.Diagnostics.HasError() {
		return
	}

	unknownLevel, d := serviceLevelUnknown(ctx, prior)
	resp.Diagnostics.Append(d...)
	fieldsDiffer, d := templateFieldsChanged(ctx, cfg.LaunchTemplate, state.LaunchTemplate, unknownLevel)
	resp.Diagnostics.Append(d...)
	doc, documentSet, d := configuredUserData(ctx, cfg.LaunchTemplate)
	resp.Diagnostics.Append(d...)
	stateDoc, stateHasDoc, d := configuredUserData(ctx, state.LaunchTemplate)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	// A document whose digest the platform already holds needs no new template
	// version: that is the case after an import, which leaves the state empty.
	userDataRolls := false
	if documentSet && (!stateHasDoc || doc != stateDoc) {
		current, err := r.client.InstanceGroups.Get(ctx, id)
		if err != nil {
			resp.Diagnostics.AddError("Reading the instance group failed", err.Error())
			return
		}
		userDataRolls = userDataDigest(doc) != current.LaunchTemplate.UserDataSHA256
	}
	templateDiffers := fieldsDiffer || userDataRolls

	bounds := changedInt(plan.MinSize, state.MinSize) || changedInt(plan.MaxSize, state.MaxSize)
	maxUnavailable := changedInt(plan.MaxUnavailablePct, state.MaxUnavailablePct)
	maxSurge := changedInt(plan.MaxSurge, state.MaxSurge)
	healthCheck := changedStr(plan.HealthCheck, state.HealthCheck)
	grace := changedInt(plan.HealthCheckGraceSeconds, state.HealthCheckGraceSeconds)
	drain := changedInt(plan.DrainSeconds, state.DrainSeconds)
	securityChanged := !sameStrings(securityGroups, stateSecurityGroups)
	poolsChanged := !sameValue(pools, statePools)
	policiesChanged := !sameValue(policies, statePolicies)
	schedulesChanged := !sameValue(schedules, stateSchedules)

	anyChange := bounds || maxUnavailable || maxSurge || healthCheck || grace || drain ||
		securityChanged || poolsChanged || policiesChanged || schedulesChanged || templateDiffers
	if anyChange && bounds {
		// A PATCH that leaves the desired capacity outside the new bounds is
		// refused, so the capacity moves first.
		fitDesiredCapacity(ctx, r.client, &resp.Diagnostics, id, knownInt(plan.MinSize), knownInt(plan.MaxSize))
		if resp.Diagnostics.HasError() {
			return
		}
	}

	// levelSent is set when the template roll carries a service level, which
	// is the configured one. It is the only way the state's level becomes known.
	levelSent := false
	if anyChange {
		var upd nemau.UpdateInstanceGroupRequest
		if bounds {
			minSize, maxSize := knownInt(plan.MinSize), knownInt(plan.MaxSize)
			upd.MinSize, upd.MaxSize = &minSize, &maxSize
		}
		if maxUnavailable {
			upd.MaxUnavailablePct = knownIntPtr(plan.MaxUnavailablePct)
		}
		if maxSurge {
			upd.MaxSurge = knownIntPtr(plan.MaxSurge)
		}
		if healthCheck {
			upd.HealthCheck = knownStr(plan.HealthCheck)
		}
		if grace {
			upd.HealthCheckGraceSeconds = knownIntPtr(plan.HealthCheckGraceSeconds)
		}
		if drain {
			upd.DrainSeconds = knownIntPtr(plan.DrainSeconds)
		}
		if securityChanged {
			upd.SecurityGroups = &securityGroups
		}
		if poolsChanged {
			upd.LoadBalancerPools = &pools
		}
		if policiesChanged {
			upd.ScalingPolicies = &policies
		}
		if schedulesChanged {
			upd.Schedules = &schedules
		}
		if templateDiffers {
			tmpl, d := templateFromConfig(ctx, cfg.LaunchTemplate)
			resp.Diagnostics.Append(d...)
			if resp.Diagnostics.HasError() {
				return
			}
			upd.LaunchTemplate = tmpl
			levelSent = tmpl != nil && tmpl.ServiceLevel != ""
		}
		runAndWait(ctx, r.client, &resp.Diagnostics, "Updating instance group", func() (*nemau.TaskRef, error) {
			return r.client.InstanceGroups.Update(ctx, id, upd)
		})
		if resp.Diagnostics.HasError() {
			return
		}
	}

	// A template change that the configuration asks to roll starts a refresh with
	// the group's stored shares. The refresh runs on the platform after this
	// point; the apply waits only for its request.
	if templateDiffers && knownBool(plan.RefreshOnTemplateChange) {
		runAndWait(ctx, r.client, &resp.Diagnostics, "Starting the rolling refresh", func() (*nemau.TaskRef, error) {
			return r.client.InstanceGroups.Refresh(ctx, id, nemau.RefreshInstanceGroupRequest{})
		})
		if resp.Diagnostics.HasError() {
			return
		}
	}

	// The desired capacity is applied when the configured value changes, and
	// not otherwise: the autoscaler's moves since then stand.
	send, now := desiredCapacityAction(priorDesired, cfg.DesiredCapacity)
	if send {
		desired := knownInt(plan.DesiredCapacity)
		runAndWait(ctx, r.client, &resp.Diagnostics, "Setting the desired capacity", func() (*nemau.TaskRef, error) {
			return r.client.InstanceGroups.SetCapacity(ctx, id, desired)
		})
		if resp.Diagnostics.HasError() {
			return
		}
	}
	if out != nil {
		resp.Diagnostics.Append(out.SetKey(ctx, desiredConfigKey, []byte(now))...)
		// The import marker ends only when this update sent the configured service
		// level: the roll that carried it has succeeded, so the state's level is
		// the configured one. An unset level is never sent, so the marker stays.
		if levelSent {
			resp.Diagnostics.Append(out.SetKey(ctx, serviceLevelUnknownKey, nil)...)
		}
	}

	g, err := r.client.InstanceGroups.Get(ctx, id)
	if err != nil {
		resp.Diagnostics.AddError("Reading the instance group failed", err.Error())
		return
	}
	plan.ID = state.ID
	resp.Diagnostics.Append(fillGroup(ctx, &plan, g, now != "")...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// fitDesiredCapacity moves the desired capacity of a group into the bounds a
// change is about to set, when the current value falls outside them. The
// current value is read from the platform, since the autoscaler moves it.
//
// The platform checks a capacity against the group's current bounds, so when
// those do not allow the move (the new bounds do not overlap the current ones),
// the bounds are first widened to the union of both ranges. The caller then
// sets the final bounds.
func fitDesiredCapacity(ctx context.Context, client *nemau.Client, diags *diag.Diagnostics, id string, minSize, maxSize int) {
	live, err := client.InstanceGroups.Get(ctx, id)
	if err != nil {
		diags.AddError("Reading the instance group failed", err.Error())
		return
	}
	if live.DesiredCapacity >= minSize && live.DesiredCapacity <= maxSize {
		return
	}
	target := min(max(live.DesiredCapacity, minSize), maxSize)
	widened := target < live.MinSize || target > live.MaxSize
	if widened {
		widenMin, widenMax := min(live.MinSize, minSize), max(live.MaxSize, maxSize)
		runAndWait(ctx, client, diags, "Widening the instance group bounds", func() (*nemau.TaskRef, error) {
			return client.InstanceGroups.Update(ctx, id, nemau.UpdateInstanceGroupRequest{MinSize: &widenMin, MaxSize: &widenMax})
		})
		if diags.HasError() {
			return
		}
	}
	runAndWait(ctx, client, diags, "Setting the desired capacity within the new bounds", func() (*nemau.TaskRef, error) {
		return client.InstanceGroups.SetCapacity(ctx, id, target)
	})
	if widened && diags.HasError() {
		// The widened bounds would stay behind a refused capacity. One PATCH puts
		// the live bounds back; its failure is reported after the capacity error,
		// which stays first.
		origMin, origMax := live.MinSize, live.MaxSize
		var restore diag.Diagnostics
		runAndWait(ctx, client, &restore, "Restoring the instance group bounds", func() (*nemau.TaskRef, error) {
			return client.InstanceGroups.Update(ctx, id, nemau.UpdateInstanceGroupRequest{MinSize: &origMin, MaxSize: &origMax})
		})
		diags.Append(restore...)
	}
}

// changedInt and changedStr report a planned value that differs from the
// state. An unknown planned value is not a change: it keeps the state's.
func changedInt(planned, state types.Int64) bool {
	return !planned.IsUnknown() && !planned.Equal(state)
}

func changedStr(planned, state types.String) bool {
	return !planned.IsUnknown() && !planned.Equal(state)
}

// ModifyPlan applies the rules that compare the plan with the state.
func (r *instanceGroupResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	var private privateKeys
	if req.Private != nil {
		private = req.Private
	}
	r.modifyPlan(ctx, req, resp, private)
}

// modifyPlan is ModifyPlan with the prior private state given apart.
func (r *instanceGroupResource) modifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse, private privateKeys) {
	// A create or a destroy has nothing to compare.
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var plan, state, cfg instanceGroupModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.planDesiredCapacity(ctx, plan, cfg, private, resp)
	if resp.Diagnostics.HasError() {
		return
	}
	if !plan.Name.Equal(state.Name) || !plan.NetworkID.Equal(state.NetworkID) || !plan.SubnetID.Equal(state.SubnetID) {
		return
	}
	if plan.LaunchTemplate.IsUnknown() {
		return
	}
	_, stateHasDoc, d := configuredUserData(ctx, state.LaunchTemplate)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() || !stateHasDoc || plan.LaunchTemplate.IsNull() {
		return
	}
	var planned launchTemplateModel
	resp.Diagnostics.Append(plan.LaunchTemplate.As(ctx, &planned, basetypes.ObjectAsOptions{})...)
	if resp.Diagnostics.HasError() || planned.UserData.IsUnknown() {
		return
	}
	if planned.UserData.IsNull() {
		// The message names the rule only: the document is never echoed.
		resp.Diagnostics.AddAttributeError(path.Root("launch_template").AtName("user_data"), "user_data cannot be removed",
			"user_data cannot be removed; set a new document or replace the group")
	}
}

// planDesiredCapacity plans a desired_capacity the configuration no longer sets
// as unknown: the platform's value then shows, instead of the value the last
// apply stored. The next apply clears the stored value.
func (r *instanceGroupResource) planDesiredCapacity(ctx context.Context, plan, cfg instanceGroupModel, private privateKeys, resp *resource.ModifyPlanResponse) {
	if !cfg.DesiredCapacity.IsNull() {
		return
	}
	stored, d := storedDesiredConfig(ctx, private)
	resp.Diagnostics.Append(d...)
	if stored == "" || resp.Diagnostics.HasError() {
		return
	}
	plan.DesiredCapacity = types.Int64Unknown()
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

func (r *instanceGroupResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state instanceGroupModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleteMembers := !state.DeleteInstancesOnDelete.IsNull() && state.DeleteInstancesOnDelete.ValueBool()
	deleteAndWaitWithHint(ctx, r.client, &resp.Diagnostics, "instance group",
		"The group still has members. Set delete_instances_on_destroy = true to delete them with the group.",
		func() (*nemau.TaskRef, error) {
			return r.client.InstanceGroups.Delete(ctx, state.ID.ValueString(), deleteMembers)
		})
}

// sameStrings compares two lists of strings, as sets of values in order.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sameValue compares two lists of SDK values; nil and empty are the same list.
func sameValue[T any](a, b []T) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// nonNil turns a nil list into an empty one. The SDK sends a pointer to an
// empty list as [], which clears the field; a pointer to nil would send null.
func nonNil[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}
