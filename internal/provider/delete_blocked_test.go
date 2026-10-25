package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

func deleteTestClient(t *testing.T, taskBody string) *nemau.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodDelete:
			_, _ = w.Write([]byte(`{"task_id":"t1","status":"pending"}`))
		case r.URL.Path == "/api/v1/tasks/t1":
			_, _ = w.Write([]byte(taskBody))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := nemau.NewClient(
		nemau.WithBaseURL(srv.URL),
		nemau.WithCredential(nemau.AccessKey{ID: "fake", Secret: "fake"}),
		nemau.WithPollInterval(time.Millisecond),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

const refusedForVolume = `{"id":"t1","taskType":"compute:delete","status":"failed","progress":0,` +
	`"error":"detach volume(s) data before deleting the instance","errorCode":"resource_in_use",` +
	`"createdAt":"2026-10-01T10:00:00Z"}`

func runDelete(t *testing.T, c *nemau.Client, hint string) diag.Diagnostics {
	t.Helper()
	var diags diag.Diagnostics
	deleteAndWaitWithHint(context.Background(), c, &diags, "instance", hint, func() (*nemau.TaskRef, error) {
		return c.Compute.DeleteInstance(context.Background(), "i-1")
	})
	return diags
}

// A delete refused because of a dependant shows the platform's message, which
// names the dependant, followed by the hint for the Terraform configuration.
func TestDeleteBlockedByDependantExplainsTheWayOut(t *testing.T) {
	diags := runDelete(t, deleteTestClient(t, refusedForVolume), "Destroy the nemau_volume_attachment first.")

	if len(diags) != 1 || !diags.HasError() {
		t.Fatalf("want exactly one error diagnostic, got %v", diags)
	}
	if !strings.Contains(diags[0].Summary(), "blocked by a resource that still uses it") {
		t.Errorf("summary does not say the delete is blocked: %q", diags[0].Summary())
	}
	for _, want := range []string{
		"detach volume(s) data before deleting the instance", // the platform's wording
		"Destroy the nemau_volume_attachment first.",         // the hint
	} {
		if !strings.Contains(diags[0].Detail(), want) {
			t.Errorf("detail is missing %q:\n%s", want, diags[0].Detail())
		}
	}
}

// Any other failure keeps the generic message with the task id, and no hint
// that could point the user at the wrong resource.
func TestOtherDeleteFailuresKeepTheTaskID(t *testing.T) {
	diags := runDelete(t, deleteTestClient(t, `{"id":"t1","taskType":"compute:delete","status":"failed","progress":0,`+
		`"error":"the operation failed; contact support with the task id","createdAt":"2026-10-01T10:00:00Z"}`),
		"Destroy the nemau_volume_attachment first.")

	if len(diags) != 1 || diags[0].Summary() != "Deleting instance failed" {
		t.Fatalf("want the generic failure, got %v", diags)
	}
	if strings.Contains(diags[0].Detail(), "nemau_volume_attachment") {
		t.Errorf("an unclassified failure was given the dependant hint:\n%s", diags[0].Detail())
	}
	if !strings.Contains(diags[0].Detail(), "Task id: t1") {
		t.Errorf("the task id is missing:\n%s", diags[0].Detail())
	}
}
