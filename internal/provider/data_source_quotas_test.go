package provider

import (
	"testing"

	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

func quotaFixture() *nemau.QuotaList {
	return &nemau.QuotaList{Quotas: []nemau.QuotaScope{
		{Scope: nemau.QuotaScopeTenant, Dimensions: []nemau.QuotaDimension{
			{Name: "cores", Unit: "count", Used: 95, Limit: 100},
			{Name: "ram", Unit: "MB", Limit: 51200},
		}},
		{Scope: nemau.QuotaScopeProject, ProjectID: "p-1", Dimensions: []nemau.QuotaDimension{
			{Name: "cores", Unit: "count", Used: 8, Reserved: 4, Limit: 32},
			{Name: "objectStorage", Unit: "bytes", Used: 4096, Limit: 107374182400},
		}},
	}}
}

// Without a scope filter each dimension appears once, for the scope that
// refuses first. A precondition written against this data source would
// otherwise read the wrong ceiling: the organization holds the larger cores
// limit and still binds, 5 left against the project's 20.
func TestQuotaEntriesReportTheBindingScope(t *testing.T) {
	entries := quotaEntries(quotaFixture(), "")

	if len(entries) != 3 {
		t.Fatalf("expected one entry per dimension, got %d", len(entries))
	}
	byName := map[string]quotaDimensionEntry{}
	for _, entry := range entries {
		if _, seen := byName[entry.Name.ValueString()]; seen {
			t.Fatalf("dimension %s reported twice", entry.Name.ValueString())
		}
		byName[entry.Name.ValueString()] = entry
	}

	cores := byName["cores"]
	if cores.Scope.ValueString() != nemau.QuotaScopeTenant {
		t.Errorf("cores bound to %q, want the organization scope", cores.Scope.ValueString())
	}
	if cores.Limit.ValueInt64() != 100 || cores.Available.ValueInt64() != 5 {
		t.Errorf("cores = limit %d, available %d", cores.Limit.ValueInt64(), cores.Available.ValueInt64())
	}
	// Reported by one scope only: that one binds.
	if byName["objectStorage"].Scope.ValueString() != nemau.QuotaScopeProject {
		t.Errorf("objectStorage scope = %q", byName["objectStorage"].Scope.ValueString())
	}
}

func TestQuotaEntriesHonourAScopeFilter(t *testing.T) {
	entries := quotaEntries(quotaFixture(), nemau.QuotaScopeProject)
	if len(entries) != 2 {
		t.Fatalf("expected the project's own dimensions, got %d", len(entries))
	}
	for _, entry := range entries {
		if entry.Scope.ValueString() != nemau.QuotaScopeProject {
			t.Errorf("entry %s came from %q", entry.Name.ValueString(), entry.Scope.ValueString())
		}
	}
	// Available still subtracts reservations inside a single scope.
	for _, entry := range entries {
		if entry.Name.ValueString() == "cores" && entry.Available.ValueInt64() != 20 {
			t.Errorf("project cores available = %d, want 20", entry.Available.ValueInt64())
		}
	}
	if len(quotaEntries(quotaFixture(), "nowhere")) != 0 {
		t.Error("an unknown scope must report nothing, not everything")
	}
}

// A credential that resolves to no project reports an empty project_id, and
// a project id only when there is one, because configurations key on it.
func TestQuotaProjectIDIsEmptyWithoutAProjectScope(t *testing.T) {
	quotas := &nemau.QuotaList{Quotas: []nemau.QuotaScope{{Scope: nemau.QuotaScopeTenant}}}
	if id := quotaProjectID(quotas); id != "" {
		t.Errorf("project id = %q, want empty", id)
	}
	if id := quotaProjectID(quotaFixture()); id != "p-1" {
		t.Errorf("project id = %q, want p-1", id)
	}
}
