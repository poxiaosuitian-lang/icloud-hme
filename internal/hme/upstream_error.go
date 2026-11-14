package hme

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// UpstreamError retains scheduling metadata without retaining response bodies,
// which can contain account identifiers, session tokens, or email addresses.
type UpstreamError struct {
	Status       int
	RateLimited  bool
	RetryAfter   time.Duration
	Operation    string
	Code         string
	ResponseKind string
}

func (e *UpstreamError) Error() string {
	if e.RateLimited {
		return fmt.Sprintf("HTTP %d: iCloud rate limited", e.Status)
	}
	return fmt.Sprintf("HTTP %d: iCloud upstream request failed", e.Status)
}

func newUpstreamError(status int, body, retryAfter string, now time.Time) *UpstreamError {
	parsed := gjson.Parse(body)
	code := parsed.Get("error.errorCode").String()
	if code == "" {
		code = parsed.Get("error.code").String()
	}
	message := strings.ToLower(parsed.Get("error.errorMessage").String())
	limited := status == http.StatusTooManyRequests
	if !parsed.Get("success").Bool() {
		limited = limited || code == "-41015" || strings.Contains(message, "too many") ||
			strings.Contains(message, "rate limit") || strings.Contains(message, "try again later") ||
			strings.Contains(message, "频繁")
	}
	kind := "other"
	if strings.TrimSpace(body) == "" {
		kind = "empty"
	} else if gjson.Valid(body) {
		kind = "json"
	} else if strings.HasPrefix(strings.ToLower(strings.TrimSpace(body)), "<!doctype html") || strings.HasPrefix(strings.ToLower(strings.TrimSpace(body)), "<html") {
		kind = "html"
	}
	return &UpstreamError{Status: status, RateLimited: limited, RetryAfter: parseRetryAfter(retryAfter, now), Code: diagnosticErrorCode(code), ResponseKind: kind}
}

// Only bounded numeric error codes are retained, never arbitrary server text.
func diagnosticErrorCode(code string) string {
	if len(code) == 0 || len(code) > 12 {
		return ""
	}
	if _, err := strconv.ParseInt(code, 10, 32); err != nil {
		return ""
	}
	return code
}

// The allowlist excludes URLs, query parameters and account identifiers.
func requestOperation(path string) string {
	switch path {
	case "/setup/ws/1/validate":
		return "validate"
	case "/v2/hme/list":
		return "list"
	case "/v1/hme/generate":
		return "generate"
	case "/v1/hme/reserve":
		return "reserve"
	case "/v1/hme/deactivate":
		return "deactivate"
	case "/v1/hme/reactivate":
		return "reactivate"
	case "/v1/hme/delete":
		return "delete"
	default:
		return "other"
	}
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		const maxSeconds = int64((1<<63 - 1) / time.Second)
		if seconds > maxSeconds {
			seconds = maxSeconds
		}
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) {
		return date.Sub(now)
	}
	return 0
}
