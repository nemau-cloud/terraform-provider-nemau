package provider

import (
	"errors"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

// The platform does not return a failed task's underlying error, so the
// diagnostic must include the task id for support, in English.
func TestTaskErrorDetailCarriesTheTaskID(t *testing.T) {
	failed := &nemau.TaskFailedError{Task: &nemau.Task{ID: "task-1"}}
	detail := taskErrorDetail(failed, nil)
	if !strings.Contains(detail, "Task id: task-1") || !strings.Contains(detail, "support") {
		t.Errorf("failed task detail = %q", detail)
	}
	detail = taskErrorDetail(errors.New("refused"), &nemau.TaskRef{TaskID: "task-2"})
	if !strings.Contains(detail, "refused") || !strings.Contains(detail, "Task id: task-2") {
		t.Errorf("task ref detail = %q", detail)
	}
}

func TestHandleReadErrorReportsAnythingButNotFound(t *testing.T) {
	var resp resource.ReadResponse
	if !handleReadError(errors.New("boom"), "load balancer", &resp) {
		t.Fatal("a read error must stop the caller")
	}
	if resp.Diagnostics.ErrorsCount() != 1 || resp.Diagnostics[0].Summary() != "Reading load balancer failed" {
		t.Errorf("diagnostics = %v", resp.Diagnostics)
	}
}
