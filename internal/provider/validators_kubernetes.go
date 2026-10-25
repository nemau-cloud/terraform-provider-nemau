package provider

import (
	"context"
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Plan plugin validators for the Kubernetes resources. They mirror the rules
// the API enforces, so a mistake fails at plan time with a message naming the
// attribute. The server stays the authority: it checks the same values again.

const (
	// maxNodePoolLabels and maxNodePoolTaints are the per-pool limits.
	maxNodePoolLabels = 16
	maxNodePoolTaints = 16
)

var (
	uuidRE           = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	nodePoolKeyRE    = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	nodePoolValueRE  = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9_.-]{0,61}[A-Za-z0-9])?$`)
	nodePoolEffects  = map[string]bool{"NoSchedule": true, "PreferNoSchedule": true, "NoExecute": true}
	nodePoolKeyRule  = "lowercase letters, digits and hyphens, 63 characters at most, starting and ending with a letter or digit"
	nodePoolValueMsg = "letters, digits, dots, hyphens and underscores, 63 characters at most, starting and ending with a letter or digit"
)

// stringCheck is a validator.String that runs one check on a known, non-null value.
type stringCheck struct {
	description string
	check       func(string) error
}

func (c stringCheck) Description(context.Context) string         { return c.description }
func (c stringCheck) MarkdownDescription(context.Context) string { return c.description }

func (c stringCheck) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if err := c.check(req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid "+req.Path.String(), err.Error())
	}
}

// listCheck is a validator.List that checks each element and the element count.
type listCheck struct {
	description string
	maxItems    int
	check       func(string) error
}

func (c listCheck) Description(context.Context) string         { return c.description }
func (c listCheck) MarkdownDescription(context.Context) string { return c.description }

func (c listCheck) ValidateList(ctx context.Context, req validator.ListRequest, resp *validator.ListResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	var items []types.String
	resp.Diagnostics.Append(req.ConfigValue.ElementsAs(ctx, &items, false)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if len(items) > c.maxItems {
		resp.Diagnostics.AddAttributeError(req.Path, "Too many entries",
			fmt.Sprintf("%s takes at most %d entries, got %d.", req.Path.String(), c.maxItems, len(items)))
	}
	for _, item := range items {
		if item.IsUnknown() || item.IsNull() {
			continue
		}
		if err := c.check(item.ValueString()); err != nil {
			resp.Diagnostics.AddAttributeError(req.Path, "Invalid "+req.Path.String(), err.Error())
		}
	}
}

// validUUID accepts the platform's identifiers: a UUID in any hex case.
func validUUID(attribute string) validator.String {
	return stringCheck{
		description: "must be a UUID",
		check: func(v string) error {
			if !uuidRE.MatchString(v) {
				return fmt.Errorf("`%s` must be a UUID, such as 0b1c2d3e-4f5a-4b6c-8d7e-9f0a1b2c3d4e", attribute)
			}
			return nil
		},
	}
}

// ipv4CIDR accepts an IPv4 prefix in CIDR notation. The range itself is the
// server's call: it refuses ranges that overlap a subnet of the project.
func ipv4CIDR(attribute string) validator.String {
	return stringCheck{
		description: "must be an IPv4 CIDR, such as 10.244.0.0/16",
		check: func(v string) error {
			prefix, err := netip.ParsePrefix(v)
			if err != nil || !prefix.Addr().Is4() {
				return fmt.Errorf("`%s` must be an IPv4 CIDR, such as 10.244.0.0/16", attribute)
			}
			return nil
		},
	}
}

// nodePoolClassOneOf accepts the one class the platform offers for node pools.
func nodePoolClassOneOf() validator.String {
	return stringCheck{
		description: "must be \"" + defaultNodeClass + "\"",
		check: func(v string) error {
			if v != defaultNodeClass {
				return fmt.Errorf("the only node class is %q, got %q", defaultNodeClass, v)
			}
			return nil
		},
	}
}

// checkNodePoolLabel accepts "key=value".
func checkNodePoolLabel(label string) error {
	key, value, found := strings.Cut(label, "=")
	if !found {
		return fmt.Errorf("label %q must be key=value", label)
	}
	if !nodePoolKeyRE.MatchString(key) {
		return fmt.Errorf("label key %q must use %s", key, nodePoolKeyRule)
	}
	if !nodePoolValueRE.MatchString(value) {
		return fmt.Errorf("label %q has an invalid value: it must use %s", key, nodePoolValueMsg)
	}
	return nil
}

// checkNodePoolTaint accepts "key[=value]:Effect".
func checkNodePoolTaint(taint string) error {
	head, effect, found := strings.Cut(taint, ":")
	if !found || !nodePoolEffects[effect] {
		return fmt.Errorf("taint %q must end in :NoSchedule, :PreferNoSchedule or :NoExecute", taint)
	}
	key, value, hasValue := strings.Cut(head, "=")
	if !nodePoolKeyRE.MatchString(key) {
		return fmt.Errorf("taint key %q must use %s", key, nodePoolKeyRule)
	}
	if hasValue && !nodePoolValueRE.MatchString(value) {
		return fmt.Errorf("taint %q has an invalid value: it must use %s", key, nodePoolValueMsg)
	}
	return nil
}

func nodePoolLabels() validator.List {
	return listCheck{
		description: "a list of key=value labels, at most 16",
		maxItems:    maxNodePoolLabels,
		check:       checkNodePoolLabel,
	}
}

func nodePoolTaints() validator.List {
	return listCheck{
		description: "a list of key[=value]:Effect taints, at most 16",
		maxItems:    maxNodePoolTaints,
		check:       checkNodePoolTaint,
	}
}
