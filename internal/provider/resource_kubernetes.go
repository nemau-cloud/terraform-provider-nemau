package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

// The API's limits. The provider checks them at plan time so that a mistake
// fails before the apply; the API checks them again.
const (
	defaultControlPlaneReplicas = 3
	maxControlPlaneReplicas     = 9
	maxNodePoolReplicas         = 100
	// defaultNodeClass is the worker class of the console's default pool.
	defaultNodeClass = "default-worker"
)

// A pool name becomes a Cluster API MachineDeployment name: a DNS label.
var nodePoolNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// sizeUnchangedUnlessSet keeps a computed size attribute stable across plans,
// then replaces the resource when the practitioner does change it.
// UseStateForUnknown has to come first: RequiresReplace treats the unknown a
// computed attribute gets during any other change as a change of its own, and
// would replace the cluster for scaling a pool.
func sizeUnchangedUnlessSet() []planmodifier.Int64 {
	return []planmodifier.Int64{int64planmodifier.UseStateForUnknown(), int64RequiresReplace()}
}

func stringUnchangedUnlessSet() []planmodifier.String {
	return []planmodifier.String{stringplanmodifier.UseStateForUnknown(), stringplanmodifier.RequiresReplace()}
}

// ---------------------------------------------------------------------------
// Kubernetes cluster
// ---------------------------------------------------------------------------

var (
	_ resource.Resource                   = (*kubernetesClusterResource)(nil)
	_ resource.ResourceWithConfigure      = (*kubernetesClusterResource)(nil)
	_ resource.ResourceWithImportState    = (*kubernetesClusterResource)(nil)
	_ resource.ResourceWithValidateConfig = (*kubernetesClusterResource)(nil)
)

func NewKubernetesClusterResource() resource.Resource { return &kubernetesClusterResource{} }

type kubernetesClusterResource struct{ client *nemau.Client }

type kubernetesClusterModel struct {
	ID                   types.String          `tfsdk:"id"`
	Name                 types.String          `tfsdk:"name"`
	KubernetesVersion    types.String          `tfsdk:"kubernetes_version"`
	ClusterClass         types.String          `tfsdk:"cluster_class"`
	ControlPlaneFlavorID types.String          `tfsdk:"control_plane_flavor_id"`
	ControlPlaneVCPUs    types.Int64           `tfsdk:"control_plane_vcpus"`
	ControlPlaneRAMMB    types.Int64           `tfsdk:"control_plane_ram_mb"`
	ControlPlaneDiskGB   types.Int64           `tfsdk:"control_plane_disk_gb"`
	ControlPlaneReplicas types.Int64           `tfsdk:"control_plane_replicas"`
	WorkerFlavorID       types.String          `tfsdk:"worker_flavor_id"`
	Phase                types.String          `tfsdk:"phase"`
	Endpoint             types.String          `tfsdk:"endpoint"`
	ControlPlaneReady    types.Bool            `tfsdk:"control_plane_ready"`
	DefaultNodePool      *defaultNodePoolModel `tfsdk:"default_node_pool"`
}

// defaultNodePoolModel is the pool created with the cluster. The API refuses a
// cluster without one; further pools are nemau_kubernetes_node_pool resources.
type defaultNodePoolModel struct {
	Name          types.String `tfsdk:"name"`
	NodeClass     types.String `tfsdk:"node_class"`
	FlavorID      types.String `tfsdk:"flavor_id"`
	VCPUs         types.Int64  `tfsdk:"vcpus"`
	RAMMB         types.Int64  `tfsdk:"ram_mb"`
	DiskGB        types.Int64  `tfsdk:"disk_gb"`
	Replicas      types.Int64  `tfsdk:"replicas"`
	AutoscalerMin types.Int64  `tfsdk:"autoscaler_min"`
	AutoscalerMax types.Int64  `tfsdk:"autoscaler_max"`
}

func (r *kubernetesClusterResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_kubernetes_cluster"
}

func (r *kubernetesClusterResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A managed Kubernetes cluster, driven by Cluster API.\n\n" +
			"The API addresses a cluster by name: `id` and `name` are the same value.\n\n" +
			"`kubernetes_version` and `control_plane_replicas` change in place. Changing " +
			"`kubernetes_version` starts a guided upgrade, which requires the " +
			"`kubernetes-guided-upgrades` organization feature: one task runs the preflight " +
			"checks, opens a maintenance window and starts the upgrade. Apply waits for that " +
			"task, which completes once the upgrade has started; the nodes keep being replaced " +
			"after apply returns. A preflight with blocking checks fails the apply and leaves " +
			"the cluster unchanged. When the version and the node counts change in the same " +
			"apply, the upgrade runs first. Changing the control-plane node size replaces the " +
			"cluster.\n\n" +
			"A cluster is created with one node pool, the `default_node_pool` block, which the " +
			"API requires. Its `replicas` and autoscaler bounds change in place; changing its " +
			"name, class or node size replaces the cluster. Declare further pools as " +
			"`nemau_kubernetes_node_pool` resources, and do not declare the default pool a " +
			"second time.\n\n" +
			"Provisioning takes several minutes, and the provider waits until the task " +
			"completes.\n\n" +
			"A cluster imports by name. An imported cluster adopts its first node pool, the one " +
			"created with it, as `default_node_pool`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Same as `name`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Cluster name. Changing it replaces the cluster.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"kubernetes_version": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Target version, `x.y.z` with or without a leading `v`. See the " +
					"`nemau_kubernetes_versions` data source. Changing it upgrades the cluster in place.",
			},
			"cluster_class": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Cluster API ClusterClass the cluster is built from. When omitted, the " +
					"platform's default class is used. Changing it replaces the cluster.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"control_plane_flavor_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Control-plane flavor id. Mutually exclusive with the custom size. Changing it replaces the cluster.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"control_plane_vcpus": schema.Int64Attribute{
				Optional: true, Computed: true,
				MarkdownDescription: "vCPUs of each control-plane node. Set it with `control_plane_ram_mb` " +
					"and `control_plane_disk_gb`, or use `control_plane_flavor_id`. Changing it replaces the cluster.",
				PlanModifiers: sizeUnchangedUnlessSet(),
			},
			"control_plane_ram_mb": schema.Int64Attribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Memory of each control-plane node, in MB. Changing it replaces the cluster.",
				PlanModifiers:       sizeUnchangedUnlessSet(),
			},
			"control_plane_disk_gb": schema.Int64Attribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Disk of each control-plane node, in GB. Changing it replaces the cluster.",
				PlanModifiers:       sizeUnchangedUnlessSet(),
			},
			"control_plane_replicas": schema.Int64Attribute{
				Optional: true, Computed: true,
				Default: int64default.StaticInt64(defaultControlPlaneReplicas),
				MarkdownDescription: "Number of control-plane nodes, 1 to 9, 3 by default as in the " +
					"console. Use an odd number: etcd needs a quorum. Changes in place.",
			},
			"worker_flavor_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Flavor id used by a `default_node_pool` that sets no size of its own. Changing it replaces the cluster.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"phase": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Lifecycle phase as reported by the platform. Shows the progress of an upgrade on the next refresh.",
			},
			"endpoint": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The API server's `host:port`, empty until it is ready.",
			},
			"control_plane_ready": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "True once the control plane is ready.",
			},
		},
		Blocks: map[string]schema.Block{
			"default_node_pool": schema.SingleNestedBlock{
				MarkdownDescription: "The node pool created with the cluster (required). The node " +
					"size is `flavor_id`, or all three of `vcpus`, `ram_mb` and `disk_gb`, or " +
					"nothing when the cluster sets `worker_flavor_id`.",
				Attributes: map[string]schema.Attribute{
					"name": schema.StringAttribute{
						Required: true,
						MarkdownDescription: "A DNS label: lowercase letters, digits and hyphens, 63 " +
							"characters at most. Changing it replaces the cluster.",
						PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
					},
					"node_class": schema.StringAttribute{
						Optional: true, Computed: true,
						MarkdownDescription: "The node class the ClusterClass defines; `" + defaultNodeClass +
							"` when not set, as in the console. Changing it replaces the cluster.",
						PlanModifiers: stringUnchangedUnlessSet(),
					},
					"flavor_id": schema.StringAttribute{
						Optional: true, Computed: true,
						MarkdownDescription: "Node flavor id. Mutually exclusive with the custom size. Changing it replaces the cluster.",
						PlanModifiers:       stringUnchangedUnlessSet(),
					},
					"vcpus": schema.Int64Attribute{
						Optional: true, Computed: true,
						MarkdownDescription: "vCPUs of each node, set together with `ram_mb` and `disk_gb`. " +
							"Changing it replaces the cluster.",
						PlanModifiers: sizeUnchangedUnlessSet(),
					},
					"ram_mb": schema.Int64Attribute{
						Optional: true, Computed: true,
						MarkdownDescription: "Memory of each node, in MB. Changing it replaces the cluster.",
						PlanModifiers:       sizeUnchangedUnlessSet(),
					},
					"disk_gb": schema.Int64Attribute{
						Optional: true, Computed: true,
						MarkdownDescription: "Disk of each node, in GB. Changing it replaces the cluster.",
						PlanModifiers:       sizeUnchangedUnlessSet(),
					},
					"replicas": schema.Int64Attribute{
						Required:            true,
						MarkdownDescription: "Number of nodes, 0 to 100. Changes in place.",
					},
					"autoscaler_min": schema.Int64Attribute{
						Optional: true,
						MarkdownDescription: "Autoscaler lower bound, at least 1. Declare it alongside " +
							"`autoscaler_max`. Changes in place.",
					},
					"autoscaler_max": schema.Int64Attribute{
						Optional: true,
						MarkdownDescription: "Autoscaler upper bound, at most 100. Declare it alongside " +
							"`autoscaler_min`. Changes in place.",
					},
				},
			},
		},
	}
}

func (r *kubernetesClusterResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromResourceProvider(req, resp)
}

func (r *kubernetesClusterResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config kubernetesClusterModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if replicas := config.ControlPlaneReplicas; !replicas.IsNull() && !replicas.IsUnknown() &&
		(replicas.ValueInt64() < 1 || replicas.ValueInt64() > maxControlPlaneReplicas) {
		resp.Diagnostics.AddAttributeError(path.Root("control_plane_replicas"), "Invalid control-plane replicas",
			fmt.Sprintf("`control_plane_replicas` must be between 1 and %d.", maxControlPlaneReplicas))
	}
	validateDefaultNodePool(config.DefaultNodePool, config.WorkerFlavorID, &resp.Diagnostics)
	if !config.ControlPlaneFlavorID.IsNull() && !config.ControlPlaneFlavorID.IsUnknown() {
		return
	}
	for _, value := range []types.Int64{
		config.ControlPlaneVCPUs, config.ControlPlaneRAMMB, config.ControlPlaneDiskGB,
	} {
		if value.IsUnknown() {
			return
		}
	}
	if config.ControlPlaneVCPUs.IsNull() || config.ControlPlaneRAMMB.IsNull() || config.ControlPlaneDiskGB.IsNull() {
		resp.Diagnostics.AddError("Incomplete control-plane size",
			"Set `control_plane_flavor_id`, or all three of `control_plane_vcpus`, "+
				"`control_plane_ram_mb` and `control_plane_disk_gb`.")
	}
}

// validateDefaultNodePool catches at plan time what the API would refuse at
// apply time. Unknown values are left to the apply.
func validateDefaultNodePool(pool *defaultNodePoolModel, workerFlavorID types.String, diags *diag.Diagnostics) {
	at := path.Root("default_node_pool")
	if pool == nil {
		diags.AddAttributeError(at, "Missing default node pool",
			"A cluster is created with at least one node pool: declare a `default_node_pool` block.")
		return
	}
	if name := pool.Name; !name.IsNull() && !name.IsUnknown() && !nodePoolNameRE.MatchString(name.ValueString()) {
		diags.AddAttributeError(at.AtName("name"), "Invalid node pool name",
			"A node pool name is a DNS label: lowercase letters, digits and hyphens, 63 characters at most.")
	}
	if replicas := pool.Replicas; !replicas.IsNull() && !replicas.IsUnknown() &&
		(replicas.ValueInt64() < 0 || replicas.ValueInt64() > maxNodePoolReplicas) {
		diags.AddAttributeError(at.AtName("replicas"), "Invalid node pool replicas",
			fmt.Sprintf("`replicas` must be between 0 and %d.", maxNodePoolReplicas))
	}
	if pool.AutoscalerMin.IsNull() != pool.AutoscalerMax.IsNull() {
		diags.AddAttributeError(at, "Incomplete autoscaler bounds",
			"`autoscaler_min` and `autoscaler_max` are declared together.")
	} else if minV, maxV := pool.AutoscalerMin, pool.AutoscalerMax; !minV.IsNull() && !minV.IsUnknown() && !maxV.IsUnknown() &&
		(minV.ValueInt64() < 1 || maxV.ValueInt64() < minV.ValueInt64() || maxV.ValueInt64() > maxNodePoolReplicas) {
		diags.AddAttributeError(at, "Invalid autoscaler bounds",
			fmt.Sprintf("The autoscaler needs 1 <= `autoscaler_min` <= `autoscaler_max` <= %d.", maxNodePoolReplicas))
	}

	sizes := []types.Int64{pool.VCPUs, pool.RAMMB, pool.DiskGB}
	for _, value := range append(sizes, types.Int64Null()) {
		if value.IsUnknown() {
			return
		}
	}
	if pool.FlavorID.IsUnknown() || workerFlavorID.IsUnknown() {
		return
	}
	set := 0
	for _, value := range sizes {
		if !value.IsNull() {
			set++
		}
	}
	switch {
	case !pool.FlavorID.IsNull() && set > 0:
		diags.AddAttributeError(at, "Conflicting node size",
			"Set either `flavor_id` or the custom size (`vcpus`, `ram_mb`, `disk_gb`), not both.")
	case set > 0 && set < len(sizes):
		diags.AddAttributeError(at, "Incomplete node size",
			"Set all three of `vcpus`, `ram_mb` and `disk_gb`, or `flavor_id`.")
	case set == 0 && pool.FlavorID.IsNull() && workerFlavorID.IsNull():
		diags.AddAttributeError(at, "Missing node size",
			"Set `flavor_id`, or all three of `vcpus`, `ram_mb` and `disk_gb`, in `default_node_pool`, "+
				"or `worker_flavor_id` on the cluster.")
	}
}

// nodePoolInit is the default pool as the create request carries it. Computed
// values still unknown read as zero and are left out of the request.
func (pool *defaultNodePoolModel) nodePoolInit() []nemau.NodePoolInit {
	if pool == nil {
		return nil
	}
	class := pool.NodeClass.ValueString()
	if class == "" {
		class = defaultNodeClass
	}
	return []nemau.NodePoolInit{{
		Name:          pool.Name.ValueString(),
		Class:         class,
		FlavorID:      pool.FlavorID.ValueString(),
		VCPUs:         int(pool.VCPUs.ValueInt64()),
		RAMMB:         int(pool.RAMMB.ValueInt64()),
		DiskGB:        int(pool.DiskGB.ValueInt64()),
		Replicas:      int(pool.Replicas.ValueInt64()),
		AutoscalerMin: int(pool.AutoscalerMin.ValueInt64()),
		AutoscalerMax: int(pool.AutoscalerMax.ValueInt64()),
	}}
}

func (r *kubernetesClusterResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan kubernetesClusterModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if plan.DefaultNodePool == nil {
		resp.Diagnostics.AddAttributeError(path.Root("default_node_pool"), "Missing default node pool",
			"A cluster is created with at least one node pool: declare a `default_node_pool` block.")
		return
	}

	name := plan.Name.ValueString()
	runAndWait(ctx, r.client, &resp.Diagnostics, "Cluster creation", func() (*nemau.TaskRef, error) {
		return r.client.Kubernetes.CreateCluster(ctx, nemau.CreateClusterRequest{
			Name:                 name,
			KubernetesVersion:    plan.KubernetesVersion.ValueString(),
			ClusterClass:         plan.ClusterClass.ValueString(),
			ControlPlaneFlavorID: plan.ControlPlaneFlavorID.ValueString(),
			ControlPlaneVCPUs:    int(plan.ControlPlaneVCPUs.ValueInt64()),
			ControlPlaneRAMMB:    int(plan.ControlPlaneRAMMB.ValueInt64()),
			ControlPlaneDiskGB:   int(plan.ControlPlaneDiskGB.ValueInt64()),
			ControlPlaneReplicas: int(plan.ControlPlaneReplicas.ValueInt64()),
			WorkerFlavorID:       plan.WorkerFlavorID.ValueString(),
			NodePools:            plan.DefaultNodePool.nodePoolInit(),
		})
	})
	if resp.Diagnostics.HasError() {
		return
	}

	// The API addresses clusters by name, so the name is the id.
	plan.ID = types.StringValue(name)
	if cluster, err := r.client.Kubernetes.GetCluster(ctx, name); err == nil {
		applyCluster(&plan, cluster)
	} else {
		plan.Phase = types.StringValue("unknown")
		plan.Endpoint = types.StringValue("")
		plan.ControlPlaneReady = types.BoolValue(false)
		fillClusterUnknowns(&plan)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *kubernetesClusterResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state kubernetesClusterModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	cluster, err := r.client.Kubernetes.GetCluster(ctx, state.Name.ValueString())
	if handleReadError(err, "Kubernetes cluster", resp) {
		return
	}
	applyCluster(&state, cluster)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *kubernetesClusterResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state kubernetesClusterModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.Name.ValueString()
	plan.ID = state.ID

	// Upgrade before scaling: an upgrade replaces every node, and scaling first
	// would create nodes on the old version only to replace them. A version
	// that differs only in spelling ("1.31.0" for "v1.31.0") is not a change.
	if !sameKubernetesVersion(state.KubernetesVersion, plan.KubernetesVersion.ValueString()) {
		runAndWait(ctx, r.client, &resp.Diagnostics, "Cluster upgrade", func() (*nemau.TaskRef, error) {
			return r.client.Kubernetes.UpgradeCluster(ctx, name, plan.KubernetesVersion.ValueString())
		})
		if resp.Diagnostics.HasError() {
			return
		}
	}

	if plan.ControlPlaneReplicas.ValueInt64() != state.ControlPlaneReplicas.ValueInt64() {
		runAndWait(ctx, r.client, &resp.Diagnostics, "Control-plane resize", func() (*nemau.TaskRef, error) {
			return r.client.Kubernetes.ScaleControlPlane(ctx, name, int(plan.ControlPlaneReplicas.ValueInt64()))
		})
		if resp.Diagnostics.HasError() {
			return
		}
	}

	// The default pool's name, class and size replace the cluster; only its
	// node count and autoscaler bounds reach here.
	if planned, current := plan.DefaultNodePool, state.DefaultNodePool; planned != nil && current != nil {
		pool := current.Name.ValueString()
		if planned.Replicas.ValueInt64() != current.Replicas.ValueInt64() {
			runAndWait(ctx, r.client, &resp.Diagnostics, "Node-pool resize", func() (*nemau.TaskRef, error) {
				return r.client.Kubernetes.ScaleNodePool(ctx, name, pool, int(planned.Replicas.ValueInt64()))
			})
			if resp.Diagnostics.HasError() {
				return
			}
		}
		if !planned.AutoscalerMin.Equal(current.AutoscalerMin) || !planned.AutoscalerMax.Equal(current.AutoscalerMax) {
			runAndWait(ctx, r.client, &resp.Diagnostics, "Autoscaler configuration", func() (*nemau.TaskRef, error) {
				return r.client.Kubernetes.SetAutoscaler(ctx, name, pool,
					int(planned.AutoscalerMin.ValueInt64()), int(planned.AutoscalerMax.ValueInt64()))
			})
			if resp.Diagnostics.HasError() {
				return
			}
		}
	}

	if cluster, err := r.client.Kubernetes.GetCluster(ctx, name); err == nil {
		applyCluster(&plan, cluster)
	} else {
		fillClusterUnknowns(&plan)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *kubernetesClusterResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state kubernetesClusterModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleteAndWait(ctx, r.client, &resp.Diagnostics, "Kubernetes cluster", func() (*nemau.TaskRef, error) {
		return r.client.Kubernetes.DeleteCluster(ctx, state.Name.ValueString())
	})
}

func (r *kubernetesClusterResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Set both id and name, as for containers: Read looks the cluster up by
	// name, so seeding only `id` would make the first Read drop the import.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, tfpathRoot("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, tfpathRoot("name"), req.ID)...)
}

// sameKubernetesVersion reports whether current is a known value naming the
// same release as reported, ignoring the optional leading "v".
func sameKubernetesVersion(current types.String, reported string) bool {
	if current.IsNull() || current.IsUnknown() {
		return false
	}
	return strings.TrimPrefix(current.ValueString(), "v") == strings.TrimPrefix(reported, "v")
}

func applyCluster(model *kubernetesClusterModel, cluster *nemau.KubernetesCluster) {
	model.ID = types.StringValue(cluster.Name)
	model.Name = types.StringValue(cluster.Name)
	model.Phase = str(cluster.Status.Phase)
	model.ControlPlaneReady = types.BoolValue(cluster.Status.ControlPlaneReady)

	topology := cluster.Spec.Topology
	// The platform stores "v1.31.2" whichever form was asked for. Keep the form
	// the configuration used when it names the same release: replacing it would
	// make Terraform fail the apply with "inconsistent result".
	if topology.Version != "" && !sameKubernetesVersion(model.KubernetesVersion, topology.Version) {
		model.KubernetesVersion = types.StringValue(topology.Version)
	}
	if topology.Class != "" {
		model.ClusterClass = types.StringValue(topology.Class)
	}
	if topology.ControlPlaneFlavorID != "" {
		model.ControlPlaneFlavorID = types.StringValue(topology.ControlPlaneFlavorID)
	}
	model.ControlPlaneVCPUs = types.Int64Value(int64(topology.ControlPlaneSizing.VCPUs))
	model.ControlPlaneRAMMB = types.Int64Value(int64(topology.ControlPlaneSizing.RAMMB))
	model.ControlPlaneDiskGB = types.Int64Value(int64(topology.ControlPlaneSizing.DiskGB))
	model.ControlPlaneReplicas = types.Int64Value(int64(topology.ControlPlaneReplicas))

	model.Endpoint = types.StringValue("")
	if endpoint := cluster.Status.ControlPlaneEndpoint; endpoint != nil && endpoint.Host != "" {
		model.Endpoint = types.StringValue(endpoint.Host + ":" + itoa32(endpoint.Port))
	}
	applyDefaultNodePool(model, topology.NodePools)
}

// applyDefaultNodePool reads the default pool back by name. An imported
// cluster has none in state yet: it adopts the first pool, the one created
// with the cluster. A pool removed outside Terraform keeps its last state.
func applyDefaultNodePool(model *kubernetesClusterModel, pools []nemau.NodePoolSpec) {
	if model.DefaultNodePool == nil {
		if len(pools) == 0 {
			return
		}
		model.DefaultNodePool = &defaultNodePoolModel{
			AutoscalerMin: types.Int64Null(), AutoscalerMax: types.Int64Null(),
		}
		applyDefaultPoolSpec(model.DefaultNodePool, pools[0])
		return
	}
	for _, pool := range pools {
		if pool.Name == model.DefaultNodePool.Name.ValueString() {
			applyDefaultPoolSpec(model.DefaultNodePool, pool)
			return
		}
	}
}

func applyDefaultPoolSpec(model *defaultNodePoolModel, pool nemau.NodePoolSpec) {
	model.Name = types.StringValue(pool.Name)
	if pool.Class != "" {
		model.NodeClass = types.StringValue(pool.Class)
	}
	model.FlavorID = types.StringNull()
	if pool.FlavorID != "" {
		model.FlavorID = types.StringValue(pool.FlavorID)
	}
	model.VCPUs = types.Int64Value(int64(pool.VCPUs))
	model.RAMMB = types.Int64Value(int64(pool.RAMMB))
	model.DiskGB = types.Int64Value(int64(pool.DiskGB))
	model.Replicas = types.Int64Value(int64(pool.Replicas))
	model.AutoscalerMin, model.AutoscalerMax = types.Int64Null(), types.Int64Null()
	if pool.AutoscalerMin != 0 || pool.AutoscalerMax != 0 {
		model.AutoscalerMin = types.Int64Value(int64(pool.AutoscalerMin))
		model.AutoscalerMax = types.Int64Value(int64(pool.AutoscalerMax))
	}
}

// fillClusterUnknowns settles computed attributes when the post-write read
// failed. Terraform errors on an unknown value left in state, so every computed
// attribute needs a concrete value even when the read failed.
func fillClusterUnknowns(model *kubernetesClusterModel) {
	for _, pair := range []struct {
		value *types.Int64
	}{
		{&model.ControlPlaneVCPUs},
		{&model.ControlPlaneRAMMB},
		{&model.ControlPlaneDiskGB},
		{&model.ControlPlaneReplicas},
	} {
		if pair.value.IsUnknown() {
			*pair.value = types.Int64Value(0)
		}
	}
	if pool := model.DefaultNodePool; pool != nil {
		settleUnknownString(&pool.NodeClass, defaultNodeClass)
		if pool.FlavorID.IsUnknown() {
			pool.FlavorID = types.StringNull()
		}
		for _, value := range []*types.Int64{&pool.VCPUs, &pool.RAMMB, &pool.DiskGB} {
			if value.IsUnknown() {
				*value = types.Int64Value(0)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Node pool
// ---------------------------------------------------------------------------

var (
	_ resource.Resource                   = (*kubernetesNodePoolResource)(nil)
	_ resource.ResourceWithConfigure      = (*kubernetesNodePoolResource)(nil)
	_ resource.ResourceWithValidateConfig = (*kubernetesNodePoolResource)(nil)
)

func NewKubernetesNodePoolResource() resource.Resource { return &kubernetesNodePoolResource{} }

type kubernetesNodePoolResource struct{ client *nemau.Client }

type kubernetesNodePoolModel struct {
	ID            types.String `tfsdk:"id"`
	ClusterName   types.String `tfsdk:"cluster_name"`
	Name          types.String `tfsdk:"name"`
	NodeClass     types.String `tfsdk:"node_class"`
	FlavorID      types.String `tfsdk:"flavor_id"`
	VCPUs         types.Int64  `tfsdk:"vcpus"`
	RAMMB         types.Int64  `tfsdk:"ram_mb"`
	DiskGB        types.Int64  `tfsdk:"disk_gb"`
	Replicas      types.Int64  `tfsdk:"replicas"`
	AutoscalerMin types.Int64  `tfsdk:"autoscaler_min"`
	AutoscalerMax types.Int64  `tfsdk:"autoscaler_max"`
}

func (r *kubernetesNodePoolResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_kubernetes_node_pool"
}

func (r *kubernetesNodePoolResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A node pool in a cluster.\n\n" +
			"`replicas` and the autoscaler bounds change in place; changing the node size " +
			"replaces the pool.\n\n" +
			"Declare additional pools with this resource rather than inside " +
			"`nemau_kubernetes_cluster`, so that adding a pool does not recreate the cluster.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Synthetic id `<cluster_name>:<name>`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"cluster_name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Name of the cluster: `nemau_kubernetes_cluster.<name>.name`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"name": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Pool name, unique in the cluster. A DNS label: lowercase letters, digits and hyphens, 63 characters at most.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"node_class": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The node class the ClusterClass defines.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"flavor_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Node flavor id. Mutually exclusive with the custom size.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"vcpus": schema.Int64Attribute{
				Optional: true, Computed: true,
				MarkdownDescription: "vCPUs of each node, set together with `ram_mb` and `disk_gb`. " +
					"Changing it replaces the pool.",
				PlanModifiers: sizeUnchangedUnlessSet(),
			},
			"ram_mb": schema.Int64Attribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Memory of each node, in MB. Changing it replaces the pool.",
				PlanModifiers:       sizeUnchangedUnlessSet(),
			},
			"disk_gb": schema.Int64Attribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Disk of each node, in GB. Changing it replaces the pool.",
				PlanModifiers:       sizeUnchangedUnlessSet(),
			},
			"replicas": schema.Int64Attribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Number of nodes. Changes in place.",
			},
			"autoscaler_min": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Autoscaler lower bound. Declare it alongside `autoscaler_max`.",
			},
			"autoscaler_max": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Autoscaler upper bound. Declare it alongside `autoscaler_min`.",
			},
		},
	}
}

func (r *kubernetesNodePoolResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromResourceProvider(req, resp)
}

func (r *kubernetesNodePoolResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config kubernetesNodePoolModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// The API requires both autoscaler bounds together and rejects a zero bound.
	// A single bound is a configuration error, so catch it at plan time.
	if config.AutoscalerMin.IsNull() != config.AutoscalerMax.IsNull() {
		resp.Diagnostics.AddError("Incomplete autoscaler bounds",
			"`autoscaler_min` and `autoscaler_max` are declared together.")
	}
	if config.FlavorID.IsNull() && !config.FlavorID.IsUnknown() {
		for _, value := range []types.Int64{config.VCPUs, config.RAMMB, config.DiskGB} {
			if value.IsUnknown() {
				return
			}
		}
		if config.VCPUs.IsNull() || config.RAMMB.IsNull() || config.DiskGB.IsNull() {
			resp.Diagnostics.AddError("Incomplete node size",
				"Set `flavor_id`, or all three of `vcpus`, `ram_mb` and `disk_gb`.")
		}
	}
}

func (r *kubernetesNodePoolResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan kubernetesNodePoolModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	cluster := plan.ClusterName.ValueString()
	runAndWait(ctx, r.client, &resp.Diagnostics, "Node-pool creation", func() (*nemau.TaskRef, error) {
		return r.client.Kubernetes.AddNodePool(ctx, cluster, nemau.AddNodePoolRequest{
			PoolName:      plan.Name.ValueString(),
			Class:         plan.NodeClass.ValueString(),
			FlavorID:      plan.FlavorID.ValueString(),
			VCPUs:         int(plan.VCPUs.ValueInt64()),
			RAMMB:         int(plan.RAMMB.ValueInt64()),
			DiskGB:        int(plan.DiskGB.ValueInt64()),
			Replicas:      int(plan.Replicas.ValueInt64()),
			AutoscalerMin: int(plan.AutoscalerMin.ValueInt64()),
			AutoscalerMax: int(plan.AutoscalerMax.ValueInt64()),
		})
	})
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(cluster + ":" + plan.Name.ValueString())
	r.fill(ctx, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *kubernetesNodePoolResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state kubernetesNodePoolModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	cluster, err := r.client.Kubernetes.GetCluster(ctx, state.ClusterName.ValueString())
	if handleReadError(err, "node pool", resp) {
		return
	}
	for _, pool := range cluster.Spec.Topology.NodePools {
		if pool.Name == state.Name.ValueString() {
			applyNodePool(&state, pool)
			resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
			return
		}
	}
	resp.State.RemoveResource(ctx)
}

func (r *kubernetesNodePoolResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state kubernetesNodePoolModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	cluster, pool := state.ClusterName.ValueString(), state.Name.ValueString()
	plan.ID = state.ID

	if plan.Replicas.ValueInt64() != state.Replicas.ValueInt64() {
		runAndWait(ctx, r.client, &resp.Diagnostics, "Node-pool resize", func() (*nemau.TaskRef, error) {
			return r.client.Kubernetes.ScaleNodePool(ctx, cluster, pool, int(plan.Replicas.ValueInt64()))
		})
		if resp.Diagnostics.HasError() {
			return
		}
	}

	if plan.AutoscalerMin != state.AutoscalerMin || plan.AutoscalerMax != state.AutoscalerMax {
		runAndWait(ctx, r.client, &resp.Diagnostics, "Autoscaler configuration", func() (*nemau.TaskRef, error) {
			return r.client.Kubernetes.SetAutoscaler(ctx, cluster, pool,
				int(plan.AutoscalerMin.ValueInt64()), int(plan.AutoscalerMax.ValueInt64()))
		})
		if resp.Diagnostics.HasError() {
			return
		}
	}

	r.fill(ctx, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *kubernetesNodePoolResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state kubernetesNodePoolModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleteAndWait(ctx, r.client, &resp.Diagnostics, "node pool", func() (*nemau.TaskRef, error) {
		return r.client.Kubernetes.RemoveNodePool(ctx,
			state.ClusterName.ValueString(), state.Name.ValueString())
	})
}

func (r *kubernetesNodePoolResource) fill(ctx context.Context, model *kubernetesNodePoolModel) {
	cluster, err := r.client.Kubernetes.GetCluster(ctx, model.ClusterName.ValueString())
	if err == nil {
		for _, pool := range cluster.Spec.Topology.NodePools {
			if pool.Name == model.Name.ValueString() {
				applyNodePool(model, pool)
				return
			}
		}
	}
	// Every computed attribute must end up concrete; unknown values left in
	// state make Terraform fail the apply.
	for _, value := range []*types.Int64{&model.VCPUs, &model.RAMMB, &model.DiskGB, &model.Replicas} {
		if value.IsUnknown() {
			*value = types.Int64Value(0)
		}
	}
}

func applyNodePool(model *kubernetesNodePoolModel, pool nemau.NodePoolSpec) {
	model.Name = types.StringValue(pool.Name)
	if pool.Class != "" {
		model.NodeClass = types.StringValue(pool.Class)
	}
	if pool.FlavorID != "" {
		model.FlavorID = types.StringValue(pool.FlavorID)
	}
	model.VCPUs = types.Int64Value(int64(pool.VCPUs))
	model.RAMMB = types.Int64Value(int64(pool.RAMMB))
	model.DiskGB = types.Int64Value(int64(pool.DiskGB))
	model.Replicas = types.Int64Value(int64(pool.Replicas))
	if pool.AutoscalerMin != 0 || pool.AutoscalerMax != 0 {
		model.AutoscalerMin = types.Int64Value(int64(pool.AutoscalerMin))
		model.AutoscalerMax = types.Int64Value(int64(pool.AutoscalerMax))
	}
}
