package provider

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	tfpath "github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

// clientFromResourceProvider extracts the configured client. A nil ProviderData
// is not an error: the framework calls Configure on resources before the
// provider itself is configured.
func clientFromResourceProvider(req resource.ConfigureRequest, resp *resource.ConfigureResponse) *nemau.Client {
	if req.ProviderData == nil {
		return nil
	}
	client, ok := req.ProviderData.(*nemau.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data",
			fmt.Sprintf("Expected *nemau.Client, got %T. This is a provider bug.", req.ProviderData))
		return nil
	}
	return client
}

func clientFromDataSourceProvider(req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) *nemau.Client {
	if req.ProviderData == nil {
		return nil
	}
	client, ok := req.ProviderData.(*nemau.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data",
			fmt.Sprintf("Expected *nemau.Client, got %T. This is a provider bug.", req.ProviderData))
		return nil
	}
	return client
}

// createAndWait submits a write, waits for it, and returns the created id.
//
// The API is asynchronous but Terraform expects a create to return a resource
// that exists. Apply waits for the task so that the next resource in the graph
// does not reference an id that does not exist yet.
func createAndWait(
	ctx context.Context,
	client *nemau.Client,
	diags *diag.Diagnostics,
	what string,
	submit func() (*nemau.TaskRef, error),
	resultKeys ...string,
) string {
	ref, err := submit()
	if err != nil {
		diags.AddError("Creating "+what+" was refused", refusalDetail(err))
		return ""
	}
	task, err := client.WaitFor(ctx, ref)
	if err != nil {
		diags.AddError("Creating "+what+" failed", taskErrorDetail(err, ref))
		return ""
	}
	if task.ResourceID != nil && *task.ResourceID != "" {
		return *task.ResourceID
	}
	if id := task.ResultString(resultKeys...); id != "" {
		return id
	}
	diags.AddError("Creating "+what+" returned no id",
		fmt.Sprintf("Task %s finished without a resource id. "+
			"Contact support with this task id.", task.ID))
	return ""
}

// runAndWait submits a write whose only output is success or failure.
func runAndWait(
	ctx context.Context,
	client *nemau.Client,
	diags *diag.Diagnostics,
	what string,
	submit func() (*nemau.TaskRef, error),
) {
	ref, err := submit()
	if err != nil {
		diags.AddError(what+" was refused", refusalDetail(err))
		return
	}
	if _, err := client.WaitFor(ctx, ref); err != nil {
		diags.AddError(what+" failed", taskErrorDetail(err, ref))
	}
}

// deleteAndWait runs a delete and treats an already-absent resource as success.
//
// A 404 on delete means the resource was removed outside Terraform. Failing the
// apply would leave a state entry that only a manual `state rm` can remove.
func deleteAndWait(
	ctx context.Context,
	client *nemau.Client,
	diags *diag.Diagnostics,
	what string,
	submit func() (*nemau.TaskRef, error),
) {
	deleteAndWaitWithHint(ctx, client, diags, what, "", submit)
}

// deleteAndWaitWithHint is deleteAndWait for a resource that other resources can
// depend on. When the platform refuses the delete because something still uses
// it, the diagnostic shows the platform's message, which names the dependant,
// followed by hint, which says what to change in the configuration.
func deleteAndWaitWithHint(
	ctx context.Context,
	client *nemau.Client,
	diags *diag.Diagnostics,
	what string,
	hint string,
	submit func() (*nemau.TaskRef, error),
) {
	ref, err := submit()
	if err != nil {
		if nemau.IsNotFound(err) {
			return
		}
		diags.AddError("Deleting "+what+" was refused", refusalDetail(err))
		return
	}
	if _, err := client.WaitFor(ctx, ref); err != nil {
		if nemau.IsResourceInUse(err) {
			diags.AddError("Deleting "+what+" is blocked by a resource that still uses it", inUseDetail(err, hint))
			return
		}
		diags.AddError("Deleting "+what+" failed", taskErrorDetail(err, ref))
	}
}

// refusalDetail explains a write the API refused before queueing it.
//
// Every write the provider makes carries its own Idempotency-Key, generated per
// operation and reused across the SDK's retries. When the platform answers a
// retry with the original task, the caller sees a normal result. A key mismatch
// is not expected; if one happens (a proxy replaying requests, a bug), explain
// it instead of showing a bare 422.
func refusalDetail(err error) string {
	if nemau.IsIdempotencyKeyMismatch(err) {
		return err.Error() + "\n\nThe platform saw the idempotency key of this operation on a different request " +
			"within the last 24 hours. Nothing was changed. Run the operation again; if it keeps failing, " +
			"report it with this message."
	}
	return err.Error()
}

// inUseDetail explains a delete refused because of a dependant: the platform's
// message first (it names what to remove), then what to change in Terraform.
func inUseDetail(err error, hint string) string {
	detail := err.Error()
	if hint != "" {
		detail += "\n\n" + hint
	}
	return detail
}

// taskErrorDetail turns a task failure into something a user can act on.
//
// The platform returns a generic message for a failed task, so the diagnostic
// includes the task id, which support needs to investigate.
func taskErrorDetail(err error, ref *nemau.TaskRef) string {
	var failed *nemau.TaskFailedError
	if errors.As(err, &failed) && failed.Task != nil {
		return fmt.Sprintf("%s\n\nTask id: %s. The platform does not return the underlying error; "+
			"give this id to support.",
			err.Error(), failed.Task.ID)
	}
	if ref != nil && ref.TaskID != "" {
		return fmt.Sprintf("%s\n\nTask id: %s", err.Error(), ref.TaskID)
	}
	return err.Error()
}

// handleReadError maps a read failure onto Terraform's Read contract: a missing
// resource is removed from state so the next plan recreates it, and any other
// failure is reported as an error.
//
// Returns true when the caller should stop (removed or errored).
func handleReadError(err error, what string, resp *resource.ReadResponse) bool {
	if err == nil {
		return false
	}
	if nemau.IsNotFound(err) {
		resp.State.RemoveResource(context.Background())
		return true
	}
	resp.Diagnostics.AddError("Reading "+what+" failed", err.Error())
	return true
}

// str converts a plain string to a framework value, mapping "" to null so an
// unset optional attribute does not read back as an empty string and produce a
// perpetual diff.
func str(value string) types.String {
	if value == "" {
		return types.StringNull()
	}
	return types.StringValue(value)
}

// strList builds a list value from strings. A nil slice becomes an empty list
// rather than null. The API does not distinguish "no addresses yet" from
// "none", and an empty list can be indexed without a null check.
func strList(values []string) types.List {
	items := make([]attr.Value, 0, len(values))
	for _, value := range values {
		items = append(items, types.StringValue(value))
	}
	list, diags := types.ListValue(types.StringType, items)
	if diags.HasError() {
		return types.ListNull(types.StringType)
	}
	return list
}

// stringsFromList reads a framework list of strings into a plain slice.
func stringsFromList(ctx context.Context, list types.List, diags *diag.Diagnostics) []string {
	if list.IsNull() || list.IsUnknown() {
		return nil
	}
	var out []string
	diags.Append(list.ElementsAs(ctx, &out, false)...)
	return out
}

// importByID is the import path for every resource whose identity is a single
// opaque id. Terraform hands the id string; the following Read fills the rest.
func importByID(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, tfpath.Root("id"), req, resp)
}

// setToStrings reads a framework set of strings into a plain slice.
//
// A null set becomes an empty slice rather than nil, because the API treats
// them differently: an empty security-group list means "stop filtering".
func setToStrings(ctx context.Context, set types.Set, diags *diag.Diagnostics) []string {
	if set.IsNull() || set.IsUnknown() {
		return []string{}
	}
	out := make([]string, 0, len(set.Elements()))
	diags.Append(set.ElementsAs(ctx, &out, false)...)
	return out
}

// tfpathRoot names a top-level attribute in a diagnostic.
func tfpathRoot(name string) tfpath.Path { return tfpath.Root(name) }

// int64RequiresReplace is the Int64 counterpart of RequiresReplace, wrapped so
// resource schemas do not each import the plan-modifier package.
func int64RequiresReplace() planmodifier.Int64 { return int64planmodifier.RequiresReplace() }

// itoa32 renders an int32 without pulling strconv into every resource file.
func itoa32(value int32) string { return strconv.FormatInt(int64(value), 10) }

// settleUnknownBool, settleUnknownString and settleUnknownInt64 give a computed
// attribute a concrete value when the post-write read failed.
//
// Terraform refuses to store an unknown value, so every computed attribute
// needs a value even when the read failed. A resource that exists must stay in
// state when a follow-up read does not answer.
func settleUnknownBool(value *types.Bool, fallback bool) {
	if value.IsUnknown() || value.IsNull() {
		*value = types.BoolValue(fallback)
	}
}

func settleUnknownString(value *types.String, fallback string) {
	if value.IsUnknown() {
		*value = types.StringValue(fallback)
	}
}

func settleUnknownInt64(value *types.Int64, fallback int64) {
	if value.IsUnknown() || value.IsNull() {
		*value = types.Int64Value(fallback)
	}
}
