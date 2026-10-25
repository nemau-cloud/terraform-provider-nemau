package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

// ---------------------------------------------------------------------------
// Load-balancer TLS certificate
// ---------------------------------------------------------------------------
//
// A certificate is immutable on the platform: every configured attribute
// forces a replacement. A renewal creates a new certificate and points the
// listener at it (tls_certificate_ref changes in place on the listener).
//
// The private key is write-only. The API never returns it, so Read keeps the
// value from state. The resource has no import, because an imported
// certificate would have no private key to plan against.

var (
	_ resource.Resource                   = (*loadBalancerCertificateResource)(nil)
	_ resource.ResourceWithConfigure      = (*loadBalancerCertificateResource)(nil)
	_ resource.ResourceWithValidateConfig = (*loadBalancerCertificateResource)(nil)
)

// A generated name is name_prefix followed by a UTC timestamp and 8 random hex
// digits, as with aws_acm_certificate and aws_iam_server_certificate. Such
// names sort by date, stay readable, and are unique enough that a replacement
// under create_before_destroy does not collide with the certificate it
// replaces.
const (
	generatedNameSuffixLength = len("20060102150405") + 8
	maxCertificateNameLength  = 255
	maxNamePrefixLength       = maxCertificateNameLength - generatedNameSuffixLength
)

func NewLoadBalancerCertificateResource() resource.Resource {
	return &loadBalancerCertificateResource{}
}

type loadBalancerCertificateResource struct{ client *nemau.Client }

type loadBalancerCertificateModel struct {
	ID                types.String `tfsdk:"id"`
	Name              types.String `tfsdk:"name"`
	NamePrefix        types.String `tfsdk:"name_prefix"`
	Certificate       types.String `tfsdk:"certificate"`
	Chain             types.String `tfsdk:"chain"`
	PrivateKey        types.String `tfsdk:"private_key"`
	FingerprintSHA256 types.String `tfsdk:"fingerprint_sha256"`
	Subject           types.String `tfsdk:"subject"`
	Domains           types.List   `tfsdk:"domains"`
	Issuer            types.String `tfsdk:"issuer"`
	NotBefore         types.String `tfsdk:"not_before"`
	NotAfter          types.String `tfsdk:"not_after"`
	KeyAlgorithm      types.String `tfsdk:"key_algorithm"`
}

func (r *loadBalancerCertificateResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_load_balancer_certificate"
}

func (r *loadBalancerCertificateResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	computed := func(description string) schema.StringAttribute {
		return schema.StringAttribute{
			Computed: true, MarkdownDescription: description,
			PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
		}
	}
	replaced := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "A TLS certificate a `TERMINATED_HTTPS` `nemau_listener` serves, named by its " +
			"`tls_certificate_ref`.\n\n" +
			"The private key is sent once and never returned by the API. It is stored in Terraform state, " +
			"so keep that state encrypted. A certificate is immutable: any change replaces it. Names are " +
			"unique in a project, so to renew without downtime set `name_prefix` rather than `name`, with " +
			"`lifecycle { create_before_destroy = true }`. The new certificate gets a new generated name, " +
			"the listener is repointed in place, and the old certificate is deleted last.\n\n" +
			"This resource cannot be imported, because an imported certificate would have no private key.",
		Attributes: map[string]schema.Attribute{
			"id": computed("Certificate ID, the value of a listener's `tls_certificate_ref`."),
			"name": schema.StringAttribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Name, unique in the project. Conflicts with `name_prefix`; one of the " +
					"two is required.",
				// A name generated from name_prefix is kept from state, or every
				// plan would show it unknown and replace the certificate.
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown(), stringplanmodifier.RequiresReplace()},
			},
			"name_prefix": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Creates a unique name beginning with this prefix: the prefix, a UTC " +
					"timestamp and 8 random hex digits. Conflicts with `name`. Use it with " +
					"`create_before_destroy` to renew a certificate without choosing a new name.",
				PlanModifiers: replaced,
			},
			"certificate": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "PEM certificate: the leaf, optionally followed by its chain " +
					"(a fullchain.pem).",
				PlanModifiers: replaced,
			},
			"chain": schema.StringAttribute{
				Optional: true, MarkdownDescription: "PEM intermediate certificates, when kept apart from `certificate`.",
				PlanModifiers: replaced,
			},
			"private_key": schema.StringAttribute{
				Required: true, Sensitive: true,
				MarkdownDescription: "PEM private key, without passphrase: RSA 2048 to 4096 bits, ECDSA " +
					"P-256 or P-384. Write-only on the API: never read back.",
				PlanModifiers: replaced,
			},
			"fingerprint_sha256": computed("SHA-256 fingerprint, in the form `openssl x509 -fingerprint -sha256` prints."),
			"subject":            computed("Subject of the certificate."),
			"domains": schema.ListAttribute{
				Computed: true, ElementType: types.StringType,
				MarkdownDescription: "Names the certificate covers (its SANs).",
				PlanModifiers:       []planmodifier.List{listplanmodifier.UseStateForUnknown()},
			},
			"issuer":        computed("Issuer of the certificate."),
			"not_before":    computed("Start of validity, RFC 3339."),
			"not_after":     computed("Expiry, RFC 3339."),
			"key_algorithm": computed("`RSA-2048` to `RSA-4096`, `ECDSA-P256` or `ECDSA-P384`."),
		},
	}
}

func (r *loadBalancerCertificateResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFromResourceProvider(req, resp)
}

func (r *loadBalancerCertificateResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan loadBalancerCertificateModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// This call is synchronous: the certificate is stored when it returns, and
	// the private key is never passed to a task.
	name := plan.Name.ValueString()
	if plan.Name.IsNull() || plan.Name.IsUnknown() {
		generated, err := generateCertificateName(plan.NamePrefix.ValueString())
		if err != nil {
			resp.Diagnostics.AddError("Generating the certificate name failed", err.Error())
			return
		}
		name = generated
	}
	cert, err := r.client.LoadBalancer.ImportCertificate(ctx, nemau.ImportCertificateRequest{
		Name:        name,
		Certificate: plan.Certificate.ValueString(),
		Chain:       plan.Chain.ValueString(),
		PrivateKey:  plan.PrivateKey.ValueString(),
	})
	if err != nil {
		resp.Diagnostics.AddError("Importing the certificate was refused", err.Error())
		return
	}
	applyLoadBalancerCertificate(&plan, cert)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *loadBalancerCertificateResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state loadBalancerCertificateModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	cert, err := r.client.LoadBalancer.GetCertificate(ctx, state.ID.ValueString())
	if handleReadError(err, "certificate", resp) {
		return
	}
	// certificate, chain and private_key stay as they are in state: the API
	// returns metadata only.
	applyLoadBalancerCertificate(&state, cert)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is never reached with a real change: every configured attribute
// forces a replacement. It only carries the plan into state.
func (r *loadBalancerCertificateResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan loadBalancerCertificateModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *loadBalancerCertificateResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state loadBalancerCertificateModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	err := r.client.LoadBalancer.DeleteCertificate(ctx, state.ID.ValueString())
	switch {
	case err == nil, nemau.IsNotFound(err):
	case nemau.IsCertificateInUse(err):
		resp.Diagnostics.AddError("The certificate is still served by a listener",
			"Point the listener's tls_certificate_ref at another certificate, or delete the listener, "+
				"before this certificate. If both are in the same configuration, reference the certificate as "+
				"nemau_load_balancer_certificate.<name>.id so Terraform orders the two.\n\n"+err.Error())
	default:
		resp.Diagnostics.AddError("Deleting the certificate failed", err.Error())
	}
}

// applyLoadBalancerCertificate copies what the API reports. The PEM attributes are
// untouched: the API never returns them.
func applyLoadBalancerCertificate(model *loadBalancerCertificateModel, cert *nemau.Certificate) {
	model.ID = types.StringValue(cert.ID)
	if cert.Name != "" {
		model.Name = types.StringValue(cert.Name)
	}
	model.FingerprintSHA256 = str(cert.FingerprintSHA256)
	model.Subject = str(cert.Subject)
	model.Domains = strList(cert.Domains)
	model.Issuer = str(cert.Issuer)
	model.NotBefore = rfc3339(cert.NotBefore)
	model.NotAfter = rfc3339(cert.NotAfter)
	model.KeyAlgorithm = str(cert.KeyAlgorithm)
}

// ValidateConfig asks for exactly one of name and name_prefix.
func (r *loadBalancerCertificateResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config loadBalancerCertificateModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	hasName := !config.Name.IsNull()
	hasPrefix := !config.NamePrefix.IsNull()
	switch {
	case hasName && hasPrefix:
		resp.Diagnostics.AddAttributeError(path.Root("name_prefix"), "Conflicting attributes",
			"Set either name or name_prefix, not both.")
	case !hasName && !hasPrefix:
		resp.Diagnostics.AddAttributeError(path.Root("name"), "Missing attribute",
			"Set name, or name_prefix to have a unique name generated.")
	case hasPrefix && !config.NamePrefix.IsUnknown() && len(config.NamePrefix.ValueString()) > maxNamePrefixLength:
		resp.Diagnostics.AddAttributeError(path.Root("name_prefix"), "name_prefix is too long",
			fmt.Sprintf("name_prefix must leave room for the generated suffix: at most %d characters.", maxNamePrefixLength))
	}
}

// generateCertificateName returns prefix + UTC timestamp + 8 random hex digits.
func generateCertificateName(prefix string) (string, error) {
	random := make([]byte, 4)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return prefix + time.Now().UTC().Format("20060102150405") + hex.EncodeToString(random), nil
}

func rfc3339(t time.Time) types.String {
	if t.IsZero() {
		return types.StringNull()
	}
	return types.StringValue(t.UTC().Format(time.RFC3339))
}
