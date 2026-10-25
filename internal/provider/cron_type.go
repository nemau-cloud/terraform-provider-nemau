package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// cronType is the type of a cron expression. Two spellings of one schedule
// that differ only in their whitespace are semantically equal: "0  2 * * *"
// and "0 2 * * *" are the same cron.
type cronType struct {
	basetypes.StringType
}

var _ basetypes.StringTypable = cronType{}

func (cronType) Equal(o attr.Type) bool {
	_, ok := o.(cronType)
	return ok
}

func (cronType) String() string {
	return "provider.cronType"
}

func (cronType) ValueFromString(_ context.Context, in basetypes.StringValue) (basetypes.StringValuable, diag.Diagnostics) {
	return cronValue{StringValue: in}, nil
}

func (t cronType) ValueFromTerraform(ctx context.Context, in tftypes.Value) (attr.Value, error) {
	attrValue, err := t.StringType.ValueFromTerraform(ctx, in)
	if err != nil {
		return nil, err
	}
	stringValue, ok := attrValue.(basetypes.StringValue)
	if !ok {
		return nil, fmt.Errorf("unexpected value type %T", attrValue)
	}
	value, diags := t.ValueFromString(ctx, stringValue)
	if diags.HasError() {
		return nil, fmt.Errorf("cron value from string: %v", diags)
	}
	return value, nil
}

func (cronType) ValueType(context.Context) attr.Value {
	return cronValue{}
}

// cronValue is a cron expression. Its semantic equality ignores the runs of
// whitespace, so that a change of spacing alone is not a change of schedule.
type cronValue struct {
	basetypes.StringValue
}

var _ basetypes.StringValuableWithSemanticEquals = cronValue{}

func (v cronValue) Equal(o attr.Value) bool {
	other, ok := o.(cronValue)
	if !ok {
		return false
	}
	return v.StringValue.Equal(other.StringValue)
}

func (cronValue) Type(context.Context) attr.Type {
	return cronType{}
}

func (v cronValue) StringSemanticEquals(_ context.Context, newValuable basetypes.StringValuable) (bool, diag.Diagnostics) {
	var diags diag.Diagnostics
	newCron, ok := newValuable.(cronValue)
	if !ok {
		diags.AddError("Semantic Equality Check Error",
			fmt.Sprintf("expected cronValue, got %T. This is a provider bug.", newValuable))
		return false, diags
	}
	if v.IsNull() || v.IsUnknown() || newCron.IsNull() || newCron.IsUnknown() {
		return v.StringValue.Equal(newCron.StringValue), diags
	}
	return normalizeCron(v.ValueString()) == normalizeCron(newCron.ValueString()), diags
}
