package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func callerUsage(t *testing.T, s *Server, query string) map[string]any {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/aliases/usage"+query, nil)
	req.Header.Set("Authorization", "Bearer "+serviceTestToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var payload struct {
		Data map[string]any `json:"data"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &payload) != nil {
		t.Fatalf("usage: %d %s", rec.Code, rec.Body.String())
	}
	return payload.Data
}

func TestCallerAllocationIndependentPoolsAndIdempotency(t *testing.T) {
	b, s := allocationServer(t)
	for _, caller := range []string{"vendor-a", "vendor-b", "default"} {
		body := fmt.Sprintf(`{"caller_id":%q,"request_id":"same-request"}`, caller)
		code, first, raw := allocationCall(s, "/api/v1/aliases", body)
		if code != 200 || first.Email != "existing@example.test" || first.CallerID != caller || b.creates != 0 {
			t.Fatalf("%s first: %d %s", caller, code, raw)
		}
		code, replay, raw := allocationCall(s, "/api/v1/aliases", body)
		if code != 200 || replay != first {
			t.Fatalf("%s replay: %d %s", caller, code, raw)
		}
		code, _, raw = allocationCall(s, "/api/v1/aliases", fmt.Sprintf(`{"caller_id":%q,"request_id":"same-request","label":"different"}`, caller))
		if code != 409 || !strings.Contains(raw, "REQUEST_ID_CONFLICT") {
			t.Fatalf("%s conflict: %d %s", caller, code, raw)
		}
	}
	code, created, raw := allocationCall(s, "/api/v1/aliases", `{"caller_id":"vendor-a","request_id":"second-request"}`)
	if code != 200 || created.Source != "created" || b.creates != 1 || created.Email == "existing@example.test" {
		t.Fatalf("caller A exhausted: %d %s", code, raw)
	}
	code, reused, raw := allocationCall(s, "/api/v1/aliases", `{"caller_id":"vendor-b","request_id":"second-request"}`)
	if code != 200 || reused.Source != "existing" || reused.Email != created.Email || b.creates != 1 {
		t.Fatalf("caller B reuses new existing: %d %s", code, raw)
	}
	loaded, err := loadAliasUsage(s.cfg.AliasUsageFile)
	if err != nil || len(loaded.claims) != 5 {
		t.Fatalf("reload: %v", err)
	}
	restarted := newWithBackend(b, s.cfg)
	restarted.aliasUsage = loaded
	code, replay, raw := allocationCall(restarted, "/api/v1/aliases", `{"caller_id":"vendor-b","request_id":"second-request"}`)
	if code != 200 || replay != reused || b.creates != 1 {
		t.Fatalf("reload replay: %d %s", code, raw)
	}
	usage := callerUsage(t, restarted, "")
	if usage["used_count"] != float64(5) || usage["unique_email_count"] != float64(2) || len(usage["callers"].([]any)) != 3 {
		t.Fatalf("aggregate usage: %#v", usage)
	}
	usage = callerUsage(t, restarted, "?caller_id=VENDOR-A")
	if usage["used_count"] != float64(2) || usage["caller_id"] != "vendor-a" {
		t.Fatalf("scoped usage: %#v", usage)
	}
}

func TestCallerImportAndExclusionsAreScoped(t *testing.T) {
	b, s := allocationServer(t)
	for i := 0; i < 2; i++ {
		code, _, raw := allocationCall(s, "/api/v1/aliases/usage", `{"caller_id":"vendor-a","entries":[{"email":"existing@example.test","claimed_at":123}]}`)
		if code != 200 {
			t.Fatalf("import: %d %s", code, raw)
		}
	}
	code, _, raw := allocationCall(s, "/api/v1/aliases", `{"caller_id":"vendor-a","auto_create":false}`)
	if code != 409 || !strings.Contains(raw, "NO_AVAILABLE_ALIAS") {
		t.Fatalf("import ignored: %d %s", code, raw)
	}
	code, result, raw := allocationCall(s, "/api/v1/aliases", `{"caller_id":"vendor-b","auto_create":false}`)
	if code != 200 || result.Email != "existing@example.test" {
		t.Fatalf("cross-caller import leaked: %d %s", code, raw)
	}
	code, _, raw = allocationCall(s, "/api/v1/aliases", `{"caller_id":"vendor-c","exclude_emails":["existing@example.test"],"auto_create":false}`)
	if code != 409 || !strings.Contains(raw, "NO_AVAILABLE_ALIAS") {
		t.Fatalf("exclusion ignored: %d %s", code, raw)
	}
	code, result, raw = allocationCall(s, "/api/v1/aliases", `{"caller_id":"vendor-d","auto_create":false}`)
	if code != 200 || result.Email != "existing@example.test" || b.creates != 0 {
		t.Fatalf("cross-caller exclusion leaked: %d %s", code, raw)
	}
	if s.aliasUsage.claims[aliasClaimKey("vendor-a", "existing@example.test")].AllocatedAt != 123 {
		t.Fatal("historical import overwritten")
	}
}

func TestCallerLegacyMigrationPreservesDefaultAndOldReplay(t *testing.T) {
	b, s := allocationServer(t)
	// Exact v1 normalization/field order; adding caller_id must not change it.
	normalized := `{"account_id":"acc_1","label":"","label_prefix":"","auto_create":true,"max_aliases":750,"exclude_emails":[],"request_id":"legacy-request"}`
	digest := sha256.Sum256([]byte(normalized))
	legacy := fmt.Sprintf(`{"version":1,"claims":{"existing@example.test":{"account_id":"acc_1","email":"existing@example.test","source":"existing","allocated_at":123,"request_id":"legacy-request","request_fingerprint":%q}}}`, hex.EncodeToString(digest[:]))
	if err := os.WriteFile(s.cfg.AliasUsageFile, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadAliasUsage(s.cfg.AliasUsageFile)
	if err != nil {
		t.Fatal(err)
	}
	s.aliasUsage = loaded
	code, replay, raw := allocationCall(s, "/api/v1/aliases", `{"account_id":"acc_1","request_id":"legacy-request"}`)
	if code != 200 || replay.Email != "existing@example.test" || replay.CallerID != "default" || replay.AllocatedAt != 123 || b.creates != 0 {
		t.Fatalf("legacy replay: %d %s", code, raw)
	}
	code, same, raw := allocationCall(s, "/api/v1/aliases", `{"account_id":"acc_1","caller_id":"DEFAULT","request_id":"legacy-request"}`)
	if code != 200 || same != replay {
		t.Fatalf("explicit default replay: %d %s", code, raw)
	}
	code, _, raw = allocationCall(s, "/api/v1/aliases", `{"auto_create":false}`)
	if code != 409 || !strings.Contains(raw, "NO_AVAILABLE_ALIAS") {
		t.Fatalf("default history lost: %d %s", code, raw)
	}
	code, reused, raw := allocationCall(s, "/api/v1/aliases", `{"caller_id":"vendor-a","request_id":"legacy-request"}`)
	if code != 200 || reused.Email != replay.Email || reused.CallerID != "vendor-a" || b.creates != 0 {
		t.Fatalf("new caller cannot reuse legacy: %d %s", code, raw)
	}
	bytes, err := os.ReadFile(s.cfg.AliasUsageFile)
	var document aliasUsageDocument
	if err != nil || json.Unmarshal(bytes, &document) != nil || document.Version != 2 || len(document.Claims) != 2 {
		t.Fatal("v2 migration did not retain both claims")
	}
	if _, err := loadAliasUsage(s.cfg.AliasUsageFile); err != nil {
		t.Fatalf("migrated ledger invalid: %v", err)
	}
}

func TestCallerConcurrentAllocationIsUniqueWithinEachCaller(t *testing.T) {
	b, s := allocationServer(t)
	b.pools["acc_1"] = append(b.pools["acc_1"], fixtureAlias("two@example.test", true), fixtureAlias("three@example.test", true), fixtureAlias("four@example.test", true))
	var wg sync.WaitGroup
	results := make(chan string, 8)
	for _, caller := range []string{"vendor-a", "vendor-b"} {
		for i := 0; i < 4; i++ {
			wg.Go(func() {
				code, result, _ := allocationCall(s, "/api/v1/aliases", fmt.Sprintf(`{"caller_id":%q,"auto_create":false}`, caller))
				if code != 200 {
					results <- "ERROR"
				} else {
					results <- aliasClaimKey(result.CallerID, result.Email)
				}
			})
		}
	}
	wg.Wait()
	close(results)
	seen := map[string]bool{}
	for key := range results {
		if key == "ERROR" || seen[key] {
			t.Fatalf("duplicate/error: %s", key)
		}
		seen[key] = true
	}
	if len(seen) != 8 || len(s.aliasUsage.claims) != 8 || b.creates != 0 {
		t.Fatal("concurrent caller allocation mismatch")
	}
}

func TestCallerValidationAndAdminUsageAuthentication(t *testing.T) {
	_, s := allocationServer(t)
	for _, caller := range []string{"bad/id", "bad|id", "标识", strings.Repeat("x", 65)} {
		for _, endpoint := range []string{"/api/v1/aliases", "/api/v1/aliases/usage"} {
			code, _, raw := allocationCall(s, endpoint, fmt.Sprintf(`{"caller_id":%q,"entries":[]}`, caller))
			if code != 400 || !strings.Contains(raw, "VALIDATION_ERROR") {
				t.Fatalf("invalid caller: %d %s", code, raw)
			}
		}
	}
	req := httptest.NewRequest("GET", "/api/settings/api/usage", nil)
	req.Header.Set("Authorization", "Bearer "+serviceTestToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatal("admin usage accessible without session")
	}
	path := filepath.Join(t.TempDir(), "invalid.json")
	for _, raw := range []string{
		`{"version":2,"claims":{"existing@example.test":{"email":"existing@example.test","caller_id":"vendor-a","allocated_at":1}}}`,
		`{"version":2,"claims":{"vendor-a|existing@example.test":{"email":"existing@example.test","caller_id":"VENDOR-A","allocated_at":1}}}`,
		`{"version":1,"claims":{"existing@example.test":{"email":"existing@example.test","caller_id":"vendor-a","allocated_at":1}}}`,
	} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadAliasUsage(path); err == nil {
			t.Fatal("invalid caller ledger accepted")
		}
	}
}

func TestCallerKeysDoNotCollideWithDefaultAddresses(t *testing.T) {
	_, s := allocationServer(t)
	if aliasClaimKey("vendor-a", "existing@example.test") == aliasClaimKey("default", "vendor-a|existing@example.test") {
		t.Fatal("caller key collides with a default-group email")
	}
	code, _, raw := allocationCall(s, "/api/v1/aliases/usage", `{"entries":[{"email":"vendor-a|existing@example.test"}]}`)
	if code != 200 {
		t.Fatalf("default import: %d %s", code, raw)
	}
	code, result, raw := allocationCall(s, "/api/v1/aliases", `{"caller_id":"vendor-a","auto_create":false}`)
	if code != 200 || result.Email != "existing@example.test" {
		t.Fatalf("default import blocks unrelated caller: %d %s", code, raw)
	}
}
