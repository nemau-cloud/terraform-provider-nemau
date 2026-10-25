package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestCronSemanticEquality(t *testing.T) {
	cases := []struct {
		name, a, b string
		want       bool
	}{
		{"extra inner spaces", "0  2 * * *", "0 2 * * *", true},
		{"leading and trailing spaces", "  0 2 * * *  ", "0 2 * * *", true},
		{"tabs and runs of spaces", "0\t2  *\t*  1-5", "0 2 * * 1-5", true},
		{"identical", "0 2 * * *", "0 2 * * *", true},
		{"different minute", "0 2 * * *", "0 3 * * *", false},
		{"different hour", "0 2 * * *", "0 2 * * 1", false},
		{"different day of week", "0 2 * * 1-5", "0 2 * * 1-6", false},
		{"different field count", "0 2 * * *", "0 2 * *", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, diags := cronValue{StringValue: types.StringValue(tc.a)}.StringSemanticEquals(
				context.Background(), cronValue{StringValue: types.StringValue(tc.b)})
			if diags.HasError() {
				t.Fatal(diags)
			}
			if got != tc.want {
				t.Errorf("StringSemanticEquals(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestCronSemanticEqualityOfNullAndUnknown(t *testing.T) {
	ctx := context.Background()
	set := cronValue{StringValue: types.StringValue("0 2 * * *")}
	for name, other := range map[string]cronValue{
		"null":    {StringValue: types.StringNull()},
		"unknown": {StringValue: types.StringUnknown()},
	} {
		if got, _ := set.StringSemanticEquals(ctx, other); got {
			t.Errorf("a set cron equals %s", name)
		}
		if got, _ := other.StringSemanticEquals(ctx, set); got {
			t.Errorf("%s equals a set cron", name)
		}
	}
	nullCron := cronValue{StringValue: types.StringNull()}
	if got, _ := nullCron.StringSemanticEquals(ctx, nullCron); !got {
		t.Error("null does not equal null")
	}
}

func TestCronTypeRoundTripsAsCronValue(t *testing.T) {
	v, diags := cronType{}.ValueFromString(context.Background(), types.StringValue("0 2 * * *"))
	if diags.HasError() {
		t.Fatal(diags)
	}
	if _, ok := v.(cronValue); !ok {
		t.Fatalf("ValueFromString returned %T, want cronValue", v)
	}
	if !(cronType{}).Equal(cronType{}) {
		t.Error("cronType does not equal itself")
	}
}
