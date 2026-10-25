package provider

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	nemau "github.com/nemau-cloud/nemau-sdk-go"
)

func TestSubnetOverlapIsExplainedOnTheRange(t *testing.T) {
	refusal := &nemau.APIError{
		StatusCode: http.StatusConflict,
		Message:    `cidr 10.0.1.0/24 overlaps subnet "app" (10.0.0.0/16) on network "backend"; the subnets of a project are routed together, so their ranges must not overlap`,
		Reason:     nemau.ReasonSubnetOverlap,
	}
	detail, overlaps := subnetOverlapDetail(refusal)
	if !overlaps {
		t.Fatal("the overlap refusal was not recognized")
	}
	if !strings.Contains(detail, `overlaps subnet "app" (10.0.0.0/16) on network "backend"`) {
		t.Errorf("detail lost the conflicting subnet: %q", detail)
	}
	if !strings.Contains(detail, "create_before_destroy") {
		t.Errorf("detail does not warn about replacing a subnet in place: %q", detail)
	}

	for _, other := range []error{
		&nemau.APIError{StatusCode: http.StatusConflict, Message: "network is in use"},
		errors.New("subnet_overlap"),
		nil,
	} {
		if _, overlaps := subnetOverlapDetail(other); overlaps {
			t.Errorf("subnetOverlapDetail(%v) claimed an overlap", other)
		}
	}
}
