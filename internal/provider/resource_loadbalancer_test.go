package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

// The platform does not change a tier in place, so a new flavor must plan a
// replacement. An omitted flavor keeps the value the API chose.
func TestLoadBalancerFlavorForcesReplacement(t *testing.T) {
	var resp resource.SchemaResponse
	NewLoadBalancerResource().Schema(context.Background(), resource.SchemaRequest{}, &resp)
	attr, ok := resp.Schema.Attributes["flavor"].(fwresource.StringAttribute)
	if !ok {
		t.Fatal("flavor attribute missing")
	}
	if !attr.Optional || !attr.Computed {
		t.Fatalf("flavor must be Optional and Computed, got optional=%v computed=%v", attr.Optional, attr.Computed)
	}
	var replaces, keepsState bool
	for _, modifier := range attr.PlanModifiers {
		description := strings.ToLower(modifier.Description(context.Background()))
		if strings.Contains(description, "destroy and recreate") {
			replaces = true
		}
		if strings.Contains(description, "will not change") {
			keepsState = true
		}
	}
	if !replaces || !keepsState {
		t.Fatalf("flavor plan modifiers: replace=%v keepState=%v", replaces, keepsState)
	}
}

func TestApplyLoadBalancerReadsTierAndSourceAddresses(t *testing.T) {
	var model loadBalancerModel
	applyLoadBalancer(&model, &nemau.LoadBalancer{ID: "lb-1", Name: "edge", VipAddress: "10.0.0.10", Flavor: "lb-ha",
		HighAvailability: &nemau.LoadBalancerHA{Status: "REDUNDANT", Appliances: []nemau.LoadBalancerAppliance{
			{Index: 1, State: "ACTIVE", Address: "10.0.0.11"}, {Index: 2, State: "STANDBY", Address: "10.0.0.12"},
		}}})
	if model.Flavor.ValueString() != "lb-ha" {
		t.Fatalf("flavor = %s", model.Flavor)
	}
	var addresses []string
	model.ApplianceAddresses.ElementsAs(context.Background(), &addresses, false)
	if len(addresses) != 2 || addresses[0] != "10.0.0.11" || addresses[1] != "10.0.0.12" {
		t.Fatalf("a pair's members see both appliances, not the VIP: %v", addresses)
	}

	// A single appliance sends from its own address, which is the VIP; a load
	// balancer the API reports without a tier reads as lb-small.
	applyLoadBalancer(&model, &nemau.LoadBalancer{ID: "lb-2", VipAddress: "10.0.0.5"})
	addresses = nil
	model.ApplianceAddresses.ElementsAs(context.Background(), &addresses, false)
	if model.Flavor.ValueString() != "lb-small" || len(addresses) != 1 || addresses[0] != "10.0.0.5" {
		t.Fatalf("single appliance: flavor=%s addresses=%v", model.Flavor, addresses)
	}
}

// The engine cannot be chosen. The attribute reports what the API runs, a
// configuration cannot set it, and an existing value is kept across plans.
func TestLoadBalancerEngineIsReadOnly(t *testing.T) {
	var resp resource.SchemaResponse
	NewLoadBalancerResource().Schema(context.Background(), resource.SchemaRequest{}, &resp)
	attr, ok := resp.Schema.Attributes["provider_engine"].(fwresource.StringAttribute)
	if !ok {
		t.Fatal("provider_engine attribute missing")
	}
	if attr.Optional || attr.Required || !attr.Computed {
		t.Fatalf("provider_engine must be Computed only, got optional=%v required=%v computed=%v",
			attr.Optional, attr.Required, attr.Computed)
	}
	for _, modifier := range attr.PlanModifiers {
		if strings.Contains(strings.ToLower(modifier.Description(context.Background())), "destroy and recreate") {
			t.Fatal("a read-only engine must never plan a replacement")
		}
	}
}

// An older load balancer keeps reporting its own engine. One the API reports
// without an engine reads as auto, because state cannot hold an unknown value.
func TestApplyLoadBalancerReadsTheEngine(t *testing.T) {
	var model loadBalancerModel
	applyLoadBalancer(&model, &nemau.LoadBalancer{ID: "lb-1", Provider: "haproxy"})
	if model.Provider.ValueString() != "haproxy" {
		t.Fatalf("engine = %s", model.Provider)
	}
	applyLoadBalancer(&model, &nemau.LoadBalancer{ID: "lb-2"})
	if model.Provider.IsUnknown() || model.Provider.ValueString() != "auto" {
		t.Fatalf("engine = %s", model.Provider)
	}
}
