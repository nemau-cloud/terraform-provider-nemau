package provider

import (
	"testing"
	"time"

	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

func TestApplySnapshotCarriesSizeAndCost(t *testing.T) {
	size, disk, cost := int64(2147483648), 20, 0.1
	var model snapshotModel
	applySnapshot(&model, nemau.Snapshot{
		ID: "s-1", Name: "before-upgrade", InstanceID: "i-1", Status: "active", Storage: "object_store",
		SizeBytes: &size, SourceDiskGB: &disk, MonthlyCost: &cost, Restorable: true,
	})
	if model.Storage.ValueString() != "object_store" || model.Status.ValueString() != "active" {
		t.Errorf("storage/status = %s/%s", model.Storage, model.Status)
	}
	if model.SizeBytes.ValueInt64() != size || model.SourceDiskGB.ValueInt64() != 20 || model.MonthlyCost.ValueFloat64() != cost {
		t.Errorf("size/disk/cost = %s/%s/%s", model.SizeBytes, model.SourceDiskGB, model.MonthlyCost)
	}
}

func TestApplySnapshotLeavesUnknownSizesNull(t *testing.T) {
	// A snapshot still uploading, or one kept on the hypervisor, has no stored
	// size: a zero would read as a free snapshot.
	var model snapshotModel
	applySnapshot(&model, nemau.Snapshot{ID: "s-2", Name: "old", InstanceID: "i-1", Status: "active", Storage: "hypervisor"})
	if !model.SizeBytes.IsNull() || !model.SourceDiskGB.IsNull() || !model.MonthlyCost.IsNull() {
		t.Errorf("unknown values should be null, got size=%s disk=%s cost=%s", model.SizeBytes, model.SourceDiskGB, model.MonthlyCost)
	}
}

func TestFindSnapshotMatchesOnID(t *testing.T) {
	items := []nemau.Snapshot{{ID: "a"}, {ID: "b", Name: "wanted"}}
	if got, ok := findSnapshot(items, "b"); !ok || got.Name != "wanted" {
		t.Errorf("findSnapshot(b) = %+v, %v", got, ok)
	}
	if _, ok := findSnapshot(items, "c"); ok {
		t.Error("findSnapshot found an id that is not listed")
	}
}

func TestApplySnapshotCarriesOriginAndExpiry(t *testing.T) {
	expires := time.Date(2026, 11, 20, 2, 0, 0, 0, time.UTC)
	var scheduled snapshotModel
	applySnapshot(&scheduled, nemau.Snapshot{ID: "s-1", Origin: nemau.SnapshotOriginScheduled, ExpiresAt: &expires})
	if scheduled.Origin.ValueString() != "scheduled" || scheduled.ExpiresAt.ValueString() != "2026-11-20T02:00:00Z" {
		t.Errorf("scheduled = origin %s, expires %s", scheduled.Origin, scheduled.ExpiresAt)
	}

	var manual snapshotModel
	applySnapshot(&manual, nemau.Snapshot{ID: "s-2", Origin: nemau.SnapshotOriginManual})
	if manual.Origin.ValueString() != "manual" || !manual.ExpiresAt.IsNull() {
		t.Errorf("manual = origin %s, expires %s; a manual snapshot must not expire", manual.Origin, manual.ExpiresAt)
	}
}
