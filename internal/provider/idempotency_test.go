package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

var uuidV4Key = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// scriptedAPI answers the writes it receives with the next scripted status,
// then 202; task reads answer a completed task naming a resource.
type scriptedAPI struct {
	mu       sync.Mutex
	statuses []int
	body     string
	keys     []string
}

func (a *scriptedAPI) client(t *testing.T) *nemau.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"id":"task-1","status":"completed","resourceId":"net-1"}`))
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		a.keys = append(a.keys, r.Header.Get("Idempotency-Key"))
		status := http.StatusAccepted
		if len(a.keys) <= len(a.statuses) {
			status = a.statuses[len(a.keys)-1]
		}
		w.WriteHeader(status)
		if status == http.StatusAccepted {
			_, _ = w.Write([]byte(`{"task_id":"task-1","status":"queued"}`))
			return
		}
		body := a.body
		if body == "" {
			body = `{"error":"service temporarily unavailable","code":"unavailable"}`
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c, err := nemau.NewClient(
		nemau.WithBaseURL(srv.URL),
		nemau.WithCredential(nemau.AccessKey{ID: "fake", Secret: "fake"}),
		nemau.WithRetry(nemau.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond}),
		nemau.WithPollInterval(time.Millisecond),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// Each operation is one logical write: one generated key, the same on every
// retry, and a different one for the next operation.
func TestOperationsCarryAKeyReusedAcrossRetries(t *testing.T) {
	api := &scriptedAPI{statuses: []int{http.StatusServiceUnavailable, http.StatusTooManyRequests}}
	client := api.client(t)
	var diags diag.Diagnostics

	id := createAndWait(context.Background(), client, &diags, "the network", func() (*nemau.TaskRef, error) {
		return client.Network.CreateNetwork(context.Background(), "prod", "")
	})
	if diags.HasError() || id != "net-1" {
		t.Fatalf("id %q, diagnostics %v", id, diags)
	}
	if len(api.keys) != 3 {
		t.Fatalf("attempts = %d, want 3 (503, 429, then 202)", len(api.keys))
	}
	first := api.keys[0]
	if !uuidV4Key.MatchString(first) || api.keys[1] != first || api.keys[2] != first {
		t.Errorf("keys = %v, want one UUID v4 on every attempt", api.keys)
	}

	runAndWait(context.Background(), client, &diags, "Network rename", func() (*nemau.TaskRef, error) {
		return client.Network.CreateNetwork(context.Background(), "other", "")
	})
	if next := api.keys[len(api.keys)-1]; next == first || !uuidV4Key.MatchString(next) {
		t.Errorf("the next operation reused key %q (first was %q)", next, first)
	}
}

func TestKeyMismatchGetsAnExplainedDiagnostic(t *testing.T) {
	api := &scriptedAPI{statuses: []int{http.StatusUnprocessableEntity}, body: `{"error":"idempotency_key_mismatch"}`}
	client := api.client(t)
	var diags diag.Diagnostics

	createAndWait(context.Background(), client, &diags, "the network", func() (*nemau.TaskRef, error) {
		return client.Network.CreateNetwork(context.Background(), "prod", "")
	})
	if !diags.HasError() {
		t.Fatal("expected an error diagnostic")
	}
	detail := diags.Errors()[0].Detail()
	if !strings.Contains(detail, "idempotency_key_mismatch") || !strings.Contains(detail, "Nothing was changed") {
		t.Errorf("detail = %q", detail)
	}
	if len(api.keys) != 1 {
		t.Errorf("a 422 was retried: %d attempts", len(api.keys))
	}
}

func TestOtherRefusalsKeepThePlainDetail(t *testing.T) {
	api := &scriptedAPI{statuses: []int{http.StatusBadRequest}, body: `{"error":"name is required"}`}
	client := api.client(t)
	var diags diag.Diagnostics
	runAndWait(context.Background(), client, &diags, "Network update", func() (*nemau.TaskRef, error) {
		return client.Network.CreateNetwork(context.Background(), "", "")
	})
	if d := diags.Errors(); len(d) != 1 || strings.Contains(d[0].Detail(), "idempotency") {
		t.Errorf("diagnostics = %v", diags)
	}
}
