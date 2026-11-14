package hme

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryAfterParsing(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"1800", 30 * time.Minute}, {now.Add(time.Hour).Format(http.TimeFormat), time.Hour},
		{"", 0}, {"-1", 0}, {"bogus", 0}, {now.Add(-time.Hour).Format(http.TimeFormat), 0},
	} {
		if got := parseRetryAfter(tc.value, now); got != tc.want {
			t.Fatalf("%q: %v want %v", tc.value, got, tc.want)
		}
	}
}

func TestUpstreamFailureDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, body, code, kind string
		status                 int
	}{
		{"forbidden", `{"success":false,"error":{"errorCode":-12345,"errorMessage":"secret@example.test TOKEN"}}`, "-12345", "json", 403},
		{"business failure", `{"success":false,"error":{"code":"-12345","errorMessage":"secret@example.test TOKEN"}}`, "-12345", "json", 200},
		{"html", `<html>secret@example.test TOKEN</html>`, "", "html", 403},
		{"empty", "", "", "empty", 403},
		{"untrusted code", `{"success":false,"error":{"code":"secret@example.test TOKEN"}}`, "", "json", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer ts.Close()
			client, err := NewClient(map[string]string{}, "icloud.com", "", false)
			if err != nil {
				t.Fatal(err)
			}
			client.serviceURL = ts.URL
			_, err = client.CreateAlias("fixture", 1)
			var upstream *UpstreamError
			if !errors.As(err, &upstream) || upstream.Operation != "generate" || upstream.Status != tc.status || upstream.Code != tc.code || upstream.ResponseKind != tc.kind {
				t.Fatalf("wrong metadata: %+v / %v", upstream, err)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "TOKEN") {
				t.Fatal("leaked upstream body")
			}
			if calls.Load() != 1 {
				t.Fatalf("unexpected retry: %d", calls.Load())
			}
		})
	}
}

func TestForbiddenGenerateNotRetried(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer ts.Close()
	client, err := NewClient(map[string]string{}, "icloud.com", "", false)
	if err != nil {
		t.Fatal(err)
	}
	client.serviceURL = ts.URL
	if _, err := client.CreateAlias("fixture", 5); err == nil {
		t.Fatal("expected error")
	}
	if calls.Load() != 1 {
		t.Fatalf("forbidden operation retried: %d", calls.Load())
	}
}

func TestDiagnosticAllowlist(t *testing.T) {
	for _, code := range []string{"TOKEN", "secret@example.test", "123\n456", "999999999999999", ""} {
		if diagnosticErrorCode(code) != "" {
			t.Fatalf("retained untrusted code %q", code)
		}
	}
	if requestOperation("/secret@example.test") != "other" {
		t.Fatal("retained arbitrary path")
	}
}

func TestUpstreamRateLimitMetadataSurvivesWithoutRetry(t *testing.T) {
	for _, tc := range []struct {
		status  int
		body    string
		limited bool
	}{
		{429, `{"success":false}`, true},
		{200, `{"success":false,"error":{"errorCode":-41015,"errorMessage":"secret"}}`, true},
		{503, `{"success":false}`, false},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			var calls atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "3600")
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer ts.Close()
			client, err := NewClient(map[string]string{}, "icloud.com", "", false)
			if err != nil {
				t.Fatal(err)
			}
			client.serviceURL = ts.URL
			_, err = client.CreateAlias("fixture", 1)
			var upstream *UpstreamError
			if !errors.As(err, &upstream) || upstream.RateLimited != tc.limited || upstream.RetryAfter != time.Hour {
				t.Fatalf("metadata lost: %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("retried during Retry-After: %d", calls.Load())
			}
		})
	}
}

func TestListAliasesRejectsBusinessFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":false,"error":{"errorMessage":"unavailable"}}`))
	}))
	defer ts.Close()
	client, err := NewClient(map[string]string{}, "icloud.com", "", false)
	if err != nil {
		t.Fatal(err)
	}
	client.serviceURL = ts.URL
	if _, err := client.ListAliases(); err == nil {
		t.Fatal("failed list accepted as empty directory")
	}
}
