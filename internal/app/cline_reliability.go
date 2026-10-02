package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// clineRateLimitScope determines whether one account can safely be replaced
// before any output has reached the caller. A supplier-wide TPM limit is not
// helped by rotating accounts that share the same upstream capacity.
type clineRateLimitScope string

const (
	clineRateLimitUnknown clineRateLimitScope = "unknown"
	clineRateLimitAccount clineRateLimitScope = "account"
	clineRateLimitShared  clineRateLimitScope = "shared"
)

const maxInitialSSEBytes = 64 << 10

// newClineAPIError keeps protocol metadata for gateway recovery while handlers
// present a fixed, non-sensitive message to downstream clients.
func newClineAPIError(status int, body, retryAfter string) *clineAPIError {
	err := &clineAPIError{
		Status:     status,
		Body:       body,
		RetryAfter: strings.TrimSpace(retryAfter),
	}
	if status == http.StatusTooManyRequests {
		err.RateLimitScope = classifyClineRateLimit(body)
	}
	return err
}

func classifyClineRateLimit(body string) clineRateLimitScope {
	normalized := strings.ToLower(body)
	// A team/region/Tokens-Per-Minute signal applies to a shared provider limit.
	// It takes precedence over a generic "try again" phrase in the same response.
	for _, marker := range []string{
		"tokens per minute", "token per minute", "per region", "team's limit",
		"team limit", "shared rate limit", "\"tpm\"", " tpm ",
	} {
		if strings.Contains(normalized, marker) {
			return clineRateLimitShared
		}
	}
	// The Cline account inference-cap response contains an explicit recovery
	// duration. Only that known shape is eligible for an account retry.
	if parseInferenceCapDuration(body) > 0 {
		return clineRateLimitAccount
	}
	return clineRateLimitUnknown
}

func hasAnotherActiveAccount(current *Account) bool {
	if current == nil {
		return false
	}
	p := loadPool()
	poolMu.Lock()
	defer poolMu.Unlock()
	for _, account := range p.Accounts {
		if account != current && account.Status == "active" {
			return true
		}
	}
	return false
}

// callClineWithAccountFailover retries once only for an account-scoped 429.
// A request has not produced a response body at this point, so retrying cannot
// duplicate text or tool effects at the downstream client.
func callClineWithAccountFailover(ctx context.Context, params map[string]any, stream, useProxies bool) (*http.Response, *Account, error) {
	response, account, err := clineRawCallFn(ctx, params, stream, useProxies)
	if err == nil {
		return response, account, nil
	}
	var apiErr *clineAPIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusTooManyRequests ||
		apiErr.RateLimitScope != clineRateLimitAccount || !hasAnotherActiveAccount(account) {
		return response, account, err
	}
	// callClineAPI already placed the constrained account in cooldown. The next
	// round-robin selection therefore picks another active account.
	return clineRawCallFn(ctx, params, stream, useProxies)
}

func clineClientErrorMessage(err error) (status int, message, kind, retryAfter string) {
	status = http.StatusBadGateway
	message = "upstream request failed"
	kind = "api_error"
	var apiErr *clineAPIError
	if !errors.As(err, &apiErr) {
		return
	}
	if apiErr.Status >= 400 && apiErr.Status <= 599 {
		status = apiErr.Status
	}
	if apiErr.RetryAfter != "" && parseRetryAfter(apiErr.RetryAfter) > 0 {
		retryAfter = apiErr.RetryAfter
	}
	switch status {
	case http.StatusTooManyRequests:
		message = "upstream rate limit reached"
		kind = "rate_limit_error"
	case http.StatusUnauthorized, http.StatusForbidden:
		message = "upstream authentication rejected"
		kind = "authentication_error"
	default:
		if status >= 500 {
			message = "upstream service failed"
		}
	}
	return
}

func writeClineAPIError(w http.ResponseWriter, err error) {
	status, message, kind, retryAfter := clineClientErrorMessage(err)
	if retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": message, "type": kind},
	})
}

type clineSSEPreflightError struct{ reason string }

func (e clineSSEPreflightError) Error() string {
	if e.reason != "" {
		return "upstream stream failed before the first event: " + e.reason
	}
	return "upstream stream failed before the first event"
}

// clineSSEPreflightCode is deliberately structural: it is safe to log and
// lets operations distinguish malformed/empty initial streams without logging
// upstream response bodies.
func clineSSEPreflightCode(err error) string {
	var preflight clineSSEPreflightError
	if errors.As(err, &preflight) && preflight.reason != "" {
		return preflight.reason
	}
	return "read_failure"
}

type replayReadCloser struct {
	io.Reader
	io.Closer
}

// boundedSSEReader prevents an upstream that never emits a newline from making
// bufio.Reader.ReadString allocate without limit while the first event is still
// being validated. The returned prefix is exactly what must be replayed.
func readInitialSSELine(reader *bufio.Reader, prefix *bytes.Buffer) (string, error) {
	var line bytes.Buffer
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(fragment) > 0 {
			if prefix.Len()+len(fragment) > maxInitialSSEBytes {
				return "", clineSSEPreflightError{reason: "initial-buffer-limit"}
			}
			prefix.Write(fragment)
			line.Write(fragment)
		}
		if err == nil {
			return line.String(), nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line.String(), err
	}
}

// prepareClineSSE waits for one complete single-line data event before HTTP
// 200 is committed. The later converters also consume one data line at a time,
// so a multi-line JSON event is rejected conservatively. It replays every byte
// consumed during preflight to the normal stream handler.
func prepareClineSSE(upstream *http.Response) error {
	if upstream == nil || upstream.Body == nil {
		return fmt.Errorf("upstream stream response is missing")
	}
	original := upstream.Body
	reader := bufio.NewReader(original)
	var prefix bytes.Buffer
	dataLineCount := 0
	dataPayload := ""
	for {
		line, err := readInitialSSELine(reader, &prefix)
		if err != nil {
			if err == io.EOF {
				return clineSSEPreflightError{reason: "eof-before-complete-event"}
			}
			return fmt.Errorf("read upstream stream before first event: %w", err)
		}

		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			// An SSE event is complete only at its blank delimiter. Do not submit
			// a client success after merely seeing a valid-looking data line.
			if dataLineCount == 0 {
				continue
			}
			if dataLineCount != 1 {
				return clineSSEPreflightError{reason: "multiple-data-lines"}
			}
			payload := dataPayload
			dataLineCount = 0
			dataPayload = ""
			if payload == "[DONE]" {
				return clineSSEPreflightError{reason: "done-before-output"}
			}
			var event map[string]any
			if json.Unmarshal([]byte(payload), &event) != nil {
				return clineSSEPreflightError{reason: "invalid-event-json"}
			}
			if sseEventHasError(event) {
				return clineSSEPreflightError{reason: "error-event"}
			}
			// A usage-only frame is not evidence that the client can consume a
			// successful stream. Keep waiting so an immediate upstream error still
			// becomes a regular HTTP error rather than a committed 200 response.
			if !sseEventHasInitialOutput(event) {
				continue
			}
			upstream.Body = &replayReadCloser{
				Reader: io.MultiReader(bytes.NewReader(prefix.Bytes()), reader),
				Closer: original,
			}
			return nil
		}
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "" {
			continue
		}
		dataLineCount++
		dataPayload = payload
	}
}

func sseEventHasError(event map[string]any) bool {
	if errValue, found := event["error"]; found && errValue != nil {
		return true
	}
	if eventType, _ := event["type"].(string); eventType == "response.failed" || eventType == "error" {
		return true
	}
	if response, ok := event["response"].(map[string]any); ok {
		if errValue, found := response["error"]; found && errValue != nil {
			return true
		}
	}
	if data, ok := event["data"].(map[string]any); ok {
		return sseEventHasError(data)
	}
	return false
}

// sseEventHasInitialOutput recognizes the OpenAI-compatible chat chunks that
// this proxy converts for chat, Responses, and Anthropic clients. A non-empty
// choices array may contain only a role delta, and is still a valid first
// output event. Empty choices and usage-only metadata must not commit 200.
func sseEventHasInitialOutput(event map[string]any) bool {
	if choices, ok := event["choices"].([]any); ok && len(choices) > 0 {
		_, valid := choices[0].(map[string]any)
		return valid
	}
	if data, ok := event["data"].(map[string]any); ok {
		if choices, ok := data["choices"].([]any); ok && len(choices) > 0 {
			_, valid := choices[0].(map[string]any)
			return valid
		}
	}
	return false
}
