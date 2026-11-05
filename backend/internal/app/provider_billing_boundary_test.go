package app

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"yingce/backend/internal/model"
)

func TestWithProviderAnalyticsUsesTaskBillingCoordinator(t *testing.T) {
	billing := newTaskBillingCoordinator(&taskBillingRepositoryStub{})
	service := &Service{taskBillingCoordinator: billing}
	task := model.Task{ID: "task-1", UserID: "user-1"}

	ctx := withProviderAnalytics(context.Background(), service, task)
	metadata, ok := ctx.Value(providerAnalyticsKey{}).(providerAnalyticsContext)
	if !ok {
		t.Fatal("withProviderAnalytics() did not attach provider metadata")
	}
	if metadata.Billing != billing {
		t.Fatalf("provider analytics billing = %p, want injected coordinator %p", metadata.Billing, billing)
	}
}

func TestSettleAudioBillingBeforeProviderRequestOnlySettlesCreate(t *testing.T) {
	repo := &taskBillingRepositoryStub{}
	billing := newTaskBillingCoordinator(repo)
	request, err := http.NewRequest(http.MethodPost, "https://provider.example/audio", nil)
	if err != nil {
		t.Fatal(err)
	}
	request = request.WithContext(context.WithValue(request.Context(), providerAnalyticsKey{}, providerAnalyticsContext{
		Billing:           billing,
		BillingOrderID:    "order-audio",
		Capability:        "audio",
		ProviderRequestID: "",
	}))

	if err := settleAudioBillingBeforeProviderRequest(request, "upload"); err != nil {
		t.Fatalf("upload settlement error = %v", err)
	}
	if len(repo.settleCalls) != 0 {
		t.Fatalf("upload must not settle audio billing: %#v", repo.settleCalls)
	}
	if err := settleAudioBillingBeforeProviderRequest(request, "create"); err != nil {
		t.Fatalf("create settlement error = %v", err)
	}
	if want := []string{"order-audio:"}; len(repo.settleCalls) != len(want) || repo.settleCalls[0] != want[0] {
		t.Fatalf("create settlement calls = %#v, want %#v", repo.settleCalls, want)
	}
}

func TestAudioBillingSettlementErrorIsUncertain(t *testing.T) {
	billing := newTaskBillingCoordinator(&taskBillingRepositoryStub{settleErr: errors.New("billing store unavailable")})
	request, err := http.NewRequest(http.MethodPost, "https://provider.example/audio", nil)
	if err != nil {
		t.Fatal(err)
	}
	request = request.WithContext(context.WithValue(request.Context(), providerAnalyticsKey{}, providerAnalyticsContext{
		Billing:        billing,
		BillingOrderID: "order-audio",
		Capability:     "audio",
	}))

	err = settleAudioBillingBeforeProviderRequest(request, "create")
	if err == nil {
		t.Fatal("settlement failure should be returned")
	}
	var settlementErr billingSettlementError
	if !errors.As(err, &settlementErr) {
		t.Fatalf("settlement error = %T %v, want billingSettlementError", err, err)
	}
	if !billingFailureUncertain(err) {
		t.Fatal("settlement failure must require billing reconciliation")
	}
}

func TestProviderExecutionPhasePreservesSubmissionEvidence(t *testing.T) {
	uploadErr := providerExecutionPhaseError{Phase: "upload", Err: providerHTTPError{StatusCode: http.StatusBadGateway}}
	if !providerRequestDefinitelyNotSubmitted(uploadErr) {
		t.Fatal("upload failure must prove create was not submitted")
	}
	if got := providerExecutionPhase(uploadErr); got != "upload" {
		t.Fatalf("upload phase = %q, want upload", got)
	}

	createErr := providerExecutionPhaseError{Phase: "create", Err: providerHTTPError{StatusCode: http.StatusBadGateway}}
	if providerRequestDefinitelyNotSubmitted(createErr) {
		t.Fatal("create transport failure must remain submission-uncertain")
	}
	var httpErr providerHTTPError
	if !errors.As(uploadErr, &httpErr) || httpErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("phase wrapper did not preserve provider HTTP error: %v", uploadErr)
	}
}

func TestRetryableIdempotentCreateExcludesUserAndUnknownTimeouts(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "bad request", err: providerHTTPError{StatusCode: http.StatusBadRequest}, want: false},
		{name: "unprocessable", err: providerHTTPError{StatusCode: http.StatusUnprocessableEntity}, want: false},
		{name: "gateway timeout", err: providerHTTPError{StatusCode: 524}, want: false},
		{name: "request timeout", err: providerHTTPError{StatusCode: http.StatusRequestTimeout}, want: true},
		{name: "service unavailable", err: providerHTTPError{StatusCode: http.StatusServiceUnavailable}, want: true},
		{name: "site overloaded", err: providerHTTPError{StatusCode: 529}, want: true},
		{name: "http version", err: providerHTTPError{StatusCode: http.StatusHTTPVersionNotSupported}, want: true},
		{name: "cloudflare ssl", err: providerHTTPError{StatusCode: 525}, want: true},
		{name: "cloudflare invalid ssl", err: providerHTTPError{StatusCode: 526}, want: true},
		{name: "user payload", err: providerHTTPError{StatusCode: http.StatusBadGateway, Body: `{"code":"invalid_parameter"}`}, want: false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := retryableIdempotentCreate(test.err); got != test.want {
				t.Fatalf("retryableIdempotentCreate(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}

func TestSafeRouteRejectionNeverTreatsGatewayTimeoutAsNoJob(t *testing.T) {
	for _, err := range []error{
		providerHTTPError{StatusCode: 524},
		providerExecutionPhaseError{Phase: "upload", Err: providerHTTPError{StatusCode: 524}},
		providerExecutionPhaseError{Phase: "create", Err: providerHTTPError{StatusCode: 524}},
	} {
		if safeRouteRejection(err) {
			t.Fatalf("safeRouteRejection(%v) = true for 524", err)
		}
	}
	if !safeRouteRejection(providerExecutionPhaseError{Phase: "upload", Err: providerHTTPError{StatusCode: http.StatusServiceUnavailable}}) {
		t.Fatal("provider upload outage should be eligible for route failover")
	}
	if safeRouteRejection(providerExecutionPhaseError{Phase: "upload", Err: providerHTTPError{StatusCode: http.StatusBadRequest, Body: `{"code":"invalid_parameter"}`}}) {
		t.Fatal("user upload input error must not trigger route failover")
	}
}

func TestProviderRequestKindCanHydrateProviderIDExcludesUpload(t *testing.T) {
	cases := []struct {
		kind string
		path string
		want bool
	}{
		{kind: "upload", path: "/v1/files", want: false},
		{kind: "create", path: "/v1/audio/generations", want: true},
		{kind: "poll", path: "/v1/tasks/id", want: true},
		{kind: "", path: "/v1/files", want: false},
		{kind: "", path: "/v1/uploads", want: false},
		{kind: "", path: "/v1/tasks/id", want: true},
	}
	for _, test := range cases {
		t.Run(test.kind+test.path, func(t *testing.T) {
			if got := providerRequestKindCanHydrateProviderID(test.kind, test.path); got != test.want {
				t.Fatalf("providerRequestKindCanHydrateProviderID(%q, %q) = %v, want %v", test.kind, test.path, got, test.want)
			}
		})
	}
}
