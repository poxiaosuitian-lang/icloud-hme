package server

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/hme"
)

func TestClassifyUpstreamCooldown(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", &hme.UpstreamError{Status: 429, RateLimited: true, RetryAfter: time.Hour})
	be := classifyUpstreamErr("创建邮箱失败", err)
	if be.Code != "UPSTREAM_RATE_LIMITED" || be.Status != 429 || be.RetryAfter != time.Hour {
		t.Fatalf("classification: %+v", be)
	}
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	backendFail(ctx, be)
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "3600" {
		t.Fatalf("response: %d %v", rec.Code, rec.Header())
	}
	be = classifyUpstreamErr("创建邮箱失败", &hme.UpstreamError{Status: 401})
	if be.Code != "UPSTREAM_UNAUTHORIZED" {
		t.Fatalf("auth classification: %+v", be)
	}
}

func TestForbiddenIsNotExpiredSessionOrAutomaticRetry(t *testing.T) {
	for _, operation := range []string{"validate", "generate", "reserve", "list"} {
		be := classifyUpstreamErr("创建邮箱失败", fmt.Errorf("wrapped: %w", &hme.UpstreamError{Status: 403, Operation: operation}))
		if be.Code != "UPSTREAM_FORBIDDEN" || be.Status != 502 || be.RetryAfter != 0 {
			t.Fatalf("incorrect forbidden classification: %+v", be)
		}
	}
	be := classifyUpstreamErr("创建邮箱失败", &hme.UpstreamError{Status: 403, RateLimited: true, RetryAfter: time.Hour})
	if be.Code != "UPSTREAM_RATE_LIMITED" || be.RetryAfter != time.Hour {
		t.Fatalf("explicit rate limit lost: %+v", be)
	}
	be = classifyUpstreamErr("创建邮箱失败", &hme.UpstreamError{Status: 200, Code: "401", Operation: "reserve"})
	if be.Code != "UPSTREAM_FAILURE" {
		t.Fatalf("business error mistaken for session failure: %+v", be)
	}
}
