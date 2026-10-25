package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

// testZoneID is a zone's public id: a UUID, not its name.
const testZoneID = "3f1c9a2e-7b4d-4e8a-9c61-2d5f0b8e4a17"

// verificationStub plays the API for a zone whose TXT record becomes visible on
// the nth check. Every check before that fails its task, the way the platform
// reports a record it cannot find.
type verificationStub struct {
	visibleOnCheck int32 // 0: never
	checks         int32
	verifyStatus   int // non-zero: answer the verify request itself with this status
	verifyBody     string
	zoneStatus     string
	onCheck        func(n int32) // called as check n arrives, before it is answered
}

func (s *verificationStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/verify"):
		if s.verifyStatus != 0 {
			w.WriteHeader(s.verifyStatus)
			_, _ = w.Write([]byte(s.verifyBody))
			return
		}
		n := atomic.AddInt32(&s.checks, 1)
		if s.onCheck != nil {
			s.onCheck(n)
		}
		w.WriteHeader(http.StatusAccepted)
		if s.visibleOnCheck != 0 && n >= s.visibleOnCheck {
			_, _ = w.Write([]byte(`{"task_id":"ok","status":"pending"}`))
			return
		}
		_, _ = w.Write([]byte(`{"task_id":"missing","status":"pending"}`))
	case r.URL.Path == "/api/v1/tasks/ok":
		_, _ = w.Write([]byte(`{"id":"ok","taskType":"dns:verify_zone","status":"completed","progress":100,` +
			`"result":{"zone_id":"` + testZoneID + `","status":"ACTIVE"}}`))
	case r.URL.Path == "/api/v1/tasks/missing":
		_, _ = w.Write([]byte(`{"id":"missing","taskType":"dns:verify_zone","status":"failed","progress":100,"error":"TXT record not found"}`))
	case r.URL.Path == "/api/v1/dns/zones":
		status := s.zoneStatus
		if status == "" {
			status = nemau.DNSZoneStatusPendingVerification
		}
		body := `{"zones":[{"id":"` + testZoneID + `","name":"example.com.","status":"` + status + `"`
		if status == nemau.DNSZoneStatusPendingVerification {
			body += `,"verification":{"name":"_nemau-challenge.example.com.","type":"TXT","value":"nemau-verification=abc123"}`
		}
		_, _ = w.Write([]byte(body + `}],"count":1}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no stub"}`))
	}
}

func stubClient(t *testing.T, h http.Handler) *nemau.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := nemau.NewClient(
		nemau.WithBaseURL(srv.URL),
		nemau.WithCredential(nemau.BearerToken("t")),
		nemau.WithRetry(nemau.RetryPolicy{MaxAttempts: 1}),
		nemau.WithPollInterval(time.Millisecond),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// A record created in the same apply is not visible at once: the check is
// asked again until it passes.
func TestVerificationRetriesUntilTheRecordIsVisible(t *testing.T) {
	stub := &verificationStub{visibleOnCheck: 3}
	c := stubClient(t, stub)

	if err := verifyZoneUntilActive(context.Background(), c, testZoneID, time.Millisecond); err != nil {
		t.Fatalf("verifyZoneUntilActive: %v", err)
	}
	if stub.checks != 3 {
		t.Errorf("checks = %d, want 3", stub.checks)
	}
}

// Applying against a zone verified outside Terraform, or re-creating the
// resource after a state loss, must not fail.
func TestVerificationOfAnActiveZoneSucceeds(t *testing.T) {
	stub := &verificationStub{
		verifyStatus: http.StatusConflict,
		verifyBody:   `{"error":"zone is already verified","reason":"dns_zone_already_verified"}`,
	}
	c := stubClient(t, stub)

	if err := verifyZoneUntilActive(context.Background(), c, testZoneID, time.Millisecond); err != nil {
		t.Fatalf("verifyZoneUntilActive: %v", err)
	}
}

// The deadline passes while the third check is in flight. The stub decides
// when, rather than a wall clock: two checks have failed by then and none
// follows, however slow the machine running the test is. verifyZoneUntilActive
// only checks whether ctx has ended, not why, so cancelling it simulates the
// timeout.
func TestVerificationGivesUpAtTheDeadline(t *testing.T) {
	ctx, expire := context.WithCancel(context.Background())
	defer expire()
	stub := &verificationStub{onCheck: func(n int32) {
		if n == 3 {
			expire()
		}
	}}
	c := stubClient(t, stub)

	err := verifyZoneUntilActive(ctx, c, testZoneID, 5*time.Millisecond)

	var expired *verificationExpiredError
	if !errors.As(err, &expired) {
		t.Fatalf("err = %v, want *verificationExpiredError", err)
	}
	if expired.last == nil || !strings.Contains(err.Error(), "TXT record not found") {
		t.Errorf("the last check's message is lost: %v", err)
	}
	if got := atomic.LoadInt32(&stub.checks); got != 3 {
		t.Errorf("checks = %d, want 3: the check is asked again until the deadline, and not after", got)
	}

	// The diagnostic names the record to publish, read with a live context.
	detail := verificationExpiredDetail(context.Background(), c, testZoneID, expired)
	for _, want := range []string{"_nemau-challenge.example.com.", "nemau-verification=abc123"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail is missing %q: %s", want, detail)
		}
	}
}

// A refusal is final, unlike a record that has not propagated yet, so the
// resource does not wait and retry.
func TestVerificationRefusalIsNotRetried(t *testing.T) {
	stub := &verificationStub{
		verifyStatus: http.StatusNotFound,
		verifyBody:   `{"error":"zone not found"}`,
	}
	c := stubClient(t, stub)

	err := verifyZoneUntilActive(context.Background(), c, "00000000-0000-4000-8000-000000000000", time.Millisecond)
	var expired *verificationExpiredError
	if err == nil || errors.As(err, &expired) || !nemau.IsNotFound(err) {
		t.Fatalf("err = %v, want the 404", err)
	}
}

func TestVerificationTimeoutMustBeADuration(t *testing.T) {
	for _, bad := range []string{"45", "soon", "-5m", "0s"} {
		if _, err := parseVerificationTimeout(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if d, err := parseVerificationTimeout(defaultVerificationTimeout); err != nil || d != 45*time.Minute {
		t.Errorf("default = %v, %v", d, err)
	}
}

// readVerification runs the resource's Read over a state holding zoneID and
// reports whether the resource stayed in the state.
func readVerification(t *testing.T, c *nemau.Client, zoneID string) bool {
	t.Helper()
	ctx := context.Background()
	res := &dnsZoneVerificationResource{client: c}
	var schemaResp resource.SchemaResponse
	res.Schema(ctx, resource.SchemaRequest{}, &schemaResp)

	state := tfsdk.State{Schema: schemaResp.Schema}
	if diags := state.Set(ctx, &dnsZoneVerificationModel{
		ID: types.StringValue(zoneID), ZoneID: types.StringValue(zoneID), Timeout: types.StringValue("45m"),
	}); diags.HasError() {
		t.Fatalf("state: %v", diags)
	}
	resp := resource.ReadResponse{State: state}
	res.Read(ctx, resource.ReadRequest{State: state}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Read: %v", resp.Diagnostics)
	}
	return !resp.State.Raw.IsNull()
}

func TestVerificationReadKeepsAnActiveZone(t *testing.T) {
	c := stubClient(t, &verificationStub{zoneStatus: nemau.DNSZoneStatusActive})
	if !readVerification(t, c, testZoneID) {
		t.Error("an active zone's verification left the state")
	}
}

// A zone found pending again must be verified again. Removing the resource
// from state makes the next plan create it.
func TestVerificationReadDropsAPendingZone(t *testing.T) {
	c := stubClient(t, &verificationStub{})
	if readVerification(t, c, testZoneID) {
		t.Error("a pending zone's verification stayed in the state")
	}
}

// The API stops returning the record once the zone is active. The attributes
// must keep their value, or a TXT record declared from them with another DNS
// provider would plan a change right after the verification.
func TestZoneVerificationAttributesSurviveTheVerification(t *testing.T) {
	model := dnsZoneModel{
		VerificationRecordName:  types.StringNull(),
		VerificationRecordValue: types.StringNull(),
	}
	applyZoneVerification(&model, &nemau.DNSZone{
		Status: nemau.DNSZoneStatusPendingVerification,
		Verification: &nemau.DNSZoneVerification{
			Name: "_nemau-challenge.example.com.", Type: "TXT", Value: "nemau-verification=abc123",
		},
	})
	applyZoneVerification(&model, &nemau.DNSZone{Status: nemau.DNSZoneStatusActive})

	if model.VerificationRecordName.ValueString() != "_nemau-challenge.example.com." ||
		model.VerificationRecordValue.ValueString() != "nemau-verification=abc123" {
		t.Errorf("verification attributes after activation = %v / %v",
			model.VerificationRecordName, model.VerificationRecordValue)
	}
}

func TestRecordSetOnPendingZoneExplainsTheDependency(t *testing.T) {
	refusal := &nemau.APIError{
		StatusCode: http.StatusConflict,
		Message:    "zone example.com. is pending verification",
		Reason:     nemau.ReasonDNSZonePendingVerification,
	}
	err := pendingZoneHint(refusal)
	if !errors.Is(err, refusal) {
		t.Errorf("the refusal is no longer wrapped: %v", err)
	}
	if !strings.Contains(err.Error(), "nemau_dns_zone_verification.<name>.zone_id") {
		t.Errorf("no hint: %v", err)
	}
	other := &nemau.APIError{StatusCode: http.StatusConflict, Message: "record set already exists"}
	if got := pendingZoneHint(other); got != other {
		t.Errorf("an unrelated refusal was rewritten: %v", got)
	}
	if pendingZoneHint(nil) != nil {
		t.Error("nil became an error")
	}
}
