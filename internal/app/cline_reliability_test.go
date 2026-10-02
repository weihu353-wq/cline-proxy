package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClineRateLimitClassification(t *testing.T) {
	cases := []struct {
		name string
		body string
		want clineRateLimitScope
	}{
		{"account inference cap", `Try again in 17h 59m`, clineRateLimitAccount},
		{"shared provider tpm", `this team's limit for input tokens per minute (per region) was reached; Retry after 44s`, clineRateLimitShared},
		{"unknown rate limit", `rate limit exceeded`, clineRateLimitUnknown},
	}
	for _, tc := range cases {
		if got := classifyClineRateLimit(tc.body); got != tc.want {
			t.Fatalf("%s: scope=%q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestWriteClineAPIErrorPreservesRateLimitMetadata(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeClineAPIError(recorder, newClineAPIError(http.StatusTooManyRequests,
		`Try again in 30m`, "44"))
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429", recorder.Code)
	}
	if got := recorder.Header().Get("Retry-After"); got != "44" {
		t.Fatalf("Retry-After=%q, want 44", got)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "rate_limit_error") || strings.Contains(body, "Try again in") {
		t.Fatalf("unsafe or incomplete public error: %s", body)
	}
}

func TestClineAccountFailoverRetriesOnceBeforeOutput(t *testing.T) {
	poolMu.Lock()
	savedPool := pool
	first := &Account{AccountID: "first", Status: "active"}
	second := &Account{AccountID: "second", Status: "active"}
	pool = &AccountPool{Accounts: []*Account{first, second}}
	poolMu.Unlock()
	defer func() {
		poolMu.Lock()
		pool = savedPool
		poolMu.Unlock()
	}()

	original := clineRawCallFn
	defer func() { clineRawCallFn = original }()
	calls := 0
	clineRawCallFn = func(context.Context, map[string]any, bool, bool) (*http.Response, *Account, error) {
		calls++
		if calls == 1 {
			first.Status = "cooldown"
			return nil, first, newClineAPIError(http.StatusTooManyRequests, `Try again in 1h`, "")
		}
		return fakeUpstream(http.StatusOK, `{}`), second, nil
	}

	response, account, err := callClineWithAccountFailover(context.Background(), map[string]any{}, false, false)
	if err != nil || response == nil || account != second || calls != 2 {
		t.Fatalf("failover response=%v account=%v calls=%d err=%v", response != nil, account, calls, err)
	}
	response.Body.Close()
}

func TestClineSharedRateLimitDoesNotRotateAccounts(t *testing.T) {
	original := clineRawCallFn
	defer func() { clineRawCallFn = original }()
	calls := 0
	clineRawCallFn = func(context.Context, map[string]any, bool, bool) (*http.Response, *Account, error) {
		calls++
		return nil, &Account{AccountID: "first", Status: "active"}, newClineAPIError(http.StatusTooManyRequests,
			`this team's limit for input tokens per minute (per region) was reached; Retry after 44s`, "44")
	}

	_, _, err := callClineWithAccountFailover(context.Background(), map[string]any{}, false, false)
	if calls != 1 {
		t.Fatalf("shared limit must not rotate accounts, calls=%d", calls)
	}
	var apiErr *clineAPIError
	if !errors.As(err, &apiErr) || apiErr.RateLimitScope != clineRateLimitShared {
		t.Fatalf("shared limit classification lost: %v", err)
	}
}

func TestPrepareClineSSERejectsErrorBeforeHeaders(t *testing.T) {
	upstream := sseResp("data: {\"error\":{\"message\":\"upstream exploded\"}}\n\n")
	if err := prepareClineSSE(upstream); err == nil {
		t.Fatal("first SSE error must fail before a success status is written")
	}
	recorder := httptest.NewRecorder()
	writeClineAPIError(recorder, clineSSEPreflightError{})
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", recorder.Code)
	}
}

func TestClineSSEPreflightCodeIsStructural(t *testing.T) {
	if got := clineSSEPreflightCode(clineSSEPreflightError{reason: "error-event"}); got != "error-event" {
		t.Fatalf("preflight code=%q, want error-event", got)
	}
	if got := clineSSEPreflightCode(errors.New("read failed")); got != "read_failure" {
		t.Fatalf("preflight fallback=%q, want read_failure", got)
	}
}

func TestPrepareClineSSEReplaysFirstEvent(t *testing.T) {
	body := ": keep-alive\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		"data: [DONE]\n\n"
	upstream := sseResp(body)
	if err := prepareClineSSE(upstream); err != nil {
		t.Fatalf("valid first event rejected: %v", err)
	}
	got, err := io.ReadAll(upstream.Body)
	if err != nil || string(got) != body {
		t.Fatalf("preflight must replay every consumed byte: got=%q err=%v", got, err)
	}
	upstream.Body.Close()
}

func TestPrepareClineSSERejectsUsageOnlyThenError(t *testing.T) {
	body := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5}}\n\n" +
		"data: {\"error\":{\"message\":\"upstream exploded\"}}\n\n"
	if err := prepareClineSSE(sseResp(body)); err == nil {
		t.Fatal("usage-only data must not commit a successful stream before a following error")
	}
}

func TestPrepareClineSSEAcceptsWrappedChoiceOutput(t *testing.T) {
	body := "data: {\"data\":{\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}}\n\n"
	upstream := sseResp(body)
	if err := prepareClineSSE(upstream); err != nil {
		t.Fatalf("wrapped choice output rejected: %v", err)
	}
	got, err := io.ReadAll(upstream.Body)
	if err != nil || string(got) != body {
		t.Fatalf("preflight must replay wrapped output: got=%q err=%v", got, err)
	}
	upstream.Body.Close()
}

func TestPrepareClineSSERejectsMalformedChoiceBeforeError(t *testing.T) {
	body := "data: {\"choices\":[null]}\n\n" +
		"data: {\"error\":{\"message\":\"upstream exploded\"}}\n\n"
	if err := prepareClineSSE(sseResp(body)); err == nil {
		t.Fatal("malformed choices must not commit a successful stream")
	}
}

func TestPrepareClineSSEAllowsNullErrorEnvelope(t *testing.T) {
	body := "data: {\"error\":null,\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"
	upstream := sseResp(body)
	if err := prepareClineSSE(upstream); err != nil {
		t.Fatalf("successful error:null envelope rejected: %v", err)
	}
	got, err := io.ReadAll(upstream.Body)
	if err != nil || string(got) != body {
		t.Fatalf("preflight must replay error:null envelope: got=%q err=%v", got, err)
	}
	upstream.Body.Close()
}

func TestPrepareClineSSERejectsUnterminatedOtherwiseValidEvent(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n"
	if err := prepareClineSSE(sseResp(body)); err == nil {
		t.Fatal("an event without its blank SSE delimiter must not commit a success")
	}
}

func TestPrepareClineSSERejectsMultiDataLineEvent(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n" +
		"data: {\"ignored\":true}\n\n"
	if err := prepareClineSSE(sseResp(body)); err == nil {
		t.Fatal("a multi-data-line event must not be accepted by single-line consumers")
	}
}

func TestPrepareClineSSEBoundsUnterminatedLine(t *testing.T) {
	upstream := sseResp(strings.Repeat("x", maxInitialSSEBytes+1))
	if err := prepareClineSSE(upstream); err == nil {
		t.Fatal("unterminated oversized upstream line must fail preflight")
	}
}

func TestResponsesStreamErrorDoesNotComplete(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		"data: {\"error\":{\"message\":\"upstream exploded\"}}\n\n"
	recorder := &flushRecorder{header: http.Header{}}
	chatStreamToResponses(recorder, sseResp(body), nil)
	got := recorder.body.String()
	if !strings.Contains(got, "response.failed") || strings.Contains(got, "response.completed") {
		t.Fatalf("Responses stream must fail without completing: %s", got)
	}
	if strings.Contains(got, "upstream exploded") {
		t.Fatalf("Responses stream leaked upstream body: %s", got)
	}
}

func TestAnthropicStreamErrorDoesNotStopSuccessfully(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		"data: {\"error\":{\"message\":\"upstream exploded\"}}\n\n"
	recorder := &flushRecorder{header: http.Header{}}
	handleAnthropicStreamWithUsage(recorder, sseResp(body), "test", nil, nil, nil)
	got := recorder.body.String()
	if !strings.Contains(got, "event: error") || strings.Contains(got, "event: message_delta") || strings.Contains(got, "event: message_stop") {
		t.Fatalf("Anthropic stream must end with error only: %s", got)
	}
	if strings.Contains(got, "upstream exploded") {
		t.Fatalf("Anthropic stream leaked upstream body: %s", got)
	}
}

func TestParseRetryAfterStillAcceptsSeconds(t *testing.T) {
	if got := parseRetryAfter("44"); got != 44*time.Second {
		t.Fatalf("Retry-After=%v, want 44s", got)
	}
}
