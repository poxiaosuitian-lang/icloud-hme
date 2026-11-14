package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
)

type allocationTestBackend struct {
	fakeBackend
	mu             sync.Mutex
	pools          map[string][]hme.Alias
	listErrors     map[string]error
	createErr      error
	creates        int
	createdAccount string
	forceEmail     string
}

func (b *allocationTestBackend) ListAliases(id string) ([]hme.Alias, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]hme.Alias(nil), b.pools[id]...), b.listErrors[id]
}
func (b *allocationTestBackend) CreateAlias(id, label string) (*hme.CreateResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.creates++
	b.createdAccount = id
	if b.createErr != nil {
		return nil, b.createErr
	}
	email := fmt.Sprintf("new-%d@example.test", b.creates)
	if b.forceEmail != "" {
		email = b.forceEmail
	}
	b.pools[id] = append(b.pools[id], hme.Alias{Email: email, AnonymousID: email, Label: label, Active: true})
	return &hme.CreateResult{Email: email, Label: label}, nil
}
func fixtureAlias(email string, active bool) hme.Alias {
	return hme.Alias{Email: email, AnonymousID: email, Active: active}
}
func allocationServer(t *testing.T) (*allocationTestBackend, *Server) {
	t.Helper()
	b := &allocationTestBackend{fakeBackend: fakeBackend{accounts: []account.Summary{
		{ID: "acc_1", ICloudEmail: "owner@icloud.com", RealEmail: "primary@example.test"},
	}}, pools: map[string][]hme.Alias{"acc_1": {fixtureAlias("existing@example.test", true)}}, listErrors: map[string]error{}}
	s := newWithBackend(b, Config{AdminPassword: "admin-pass-2026-strong", APIToken: serviceTestToken, AliasUsageFile: filepath.Join(t.TempDir(), "usage.json")})
	return b, s
}
func allocationCall(s *Server, path, body string) (int, aliasAllocation, string) {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+serviceTestToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var payload struct {
		Data aliasAllocation `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	return rec.Code, payload.Data, rec.Body.String()
}
func TestAllocationExistingThenOneNew(t *testing.T) {
	b, s := allocationServer(t)
	code, result, response := allocationCall(s, "/api/v1/aliases", `{"account_id":"acc_1"}`)
	if code != 200 || result.Source != "existing" || result.Email != "existing@example.test" || b.creates != 0 {
		t.Fatalf("existing: %d %s creates=%d", code, response, b.creates)
	}
	code, result, response = allocationCall(s, "/api/v1/aliases/allocate", `{"account_id":"acc_1","label":"API"}`)
	if code != 200 || result.Source != "created" || result.Label != "API" || b.creates != 1 {
		t.Fatalf("created: %d %s creates=%d", code, response, b.creates)
	}
	if strings.Contains(response, "request_fingerprint") || strings.Contains(response, serviceTestToken) {
		t.Fatal("internal metadata exposed")
	}
}
func TestAllocationExcludesPrimaryInactiveMalformedAndUsed(t *testing.T) {
	b, s := allocationServer(t)
	b.pools["acc_1"] = []hme.Alias{fixtureAlias("owner@icloud.com", true), fixtureAlias("primary@example.test", true), fixtureAlias("inactive@example.test", false), fixtureAlias("malformed", true), fixtureAlias("used@example.test", true), {Email: "missing-id@example.test", Active: true}, fixtureAlias("unused@example.test", true)}
	code, result, response := allocationCall(s, "/api/v1/aliases", `{"exclude_emails":["USED@example.test"],"auto_create":false}`)
	if code != 200 || result.Email != "unused@example.test" || b.creates != 0 {
		t.Fatalf("%d %s", code, response)
	}
	if len(s.aliasUsage.claims) != 2 {
		t.Fatal("excluded address was not durably imported")
	}
}
func TestAllocationSearchesAllAccountsBeforeCreation(t *testing.T) {
	b, s := allocationServer(t)
	b.accounts = append(b.accounts, account.Summary{ID: "acc_2", ICloudEmail: "second-owner@icloud.com"})
	b.pools["acc_1"] = []hme.Alias{}
	b.pools["acc_2"] = []hme.Alias{fixtureAlias("second@example.test", true)}
	code, result, response := allocationCall(s, "/api/v1/aliases", `{}`)
	if code != 200 || result.AccountID != "acc_2" || b.creates != 0 {
		t.Fatalf("%d %s", code, response)
	}
}
func TestAllocationSelectedAccountAndAllPrimaryExclusion(t *testing.T) {
	b, s := allocationServer(t)
	b.accounts = append(b.accounts, account.Summary{ID: "acc_2", ICloudEmail: "second-owner@icloud.com"})
	b.pools["acc_1"] = []hme.Alias{fixtureAlias("second-owner@icloud.com", true), fixtureAlias("selected@example.test", true)}
	b.pools["acc_2"] = []hme.Alias{fixtureAlias("other@example.test", true)}
	code, result, response := allocationCall(s, "/api/v1/aliases", `{"account_id":"acc_1","auto_create":false}`)
	if code != 200 || result.Email != "selected@example.test" {
		t.Fatalf("%d %s", code, response)
	}
	code, _, response = allocationCall(s, "/api/v1/aliases", `{"account_id":"missing"}`)
	if code != 404 || !strings.Contains(response, "ACCOUNT_NOT_FOUND") {
		t.Fatalf("%d %s", code, response)
	}
}
func TestAllocationQuotaIncludesInactiveButAllowsExisting(t *testing.T) {
	b, s := allocationServer(t)
	b.pools["acc_1"] = make([]hme.Alias, 750)
	b.pools["acc_1"][749] = fixtureAlias("last@example.test", true)
	code, _, response := allocationCall(s, "/api/v1/aliases", `{}`)
	if code != 200 {
		t.Fatalf("existing at quota: %d %s", code, response)
	}
	code, _, response = allocationCall(s, "/api/v1/aliases", `{}`)
	if code != 409 || !strings.Contains(response, "ALIAS_LIMIT_REACHED") || b.creates != 0 {
		t.Fatalf("quota: %d %s", code, response)
	}
}
func TestAllocationNoAutoCreateAndCustomQuota(t *testing.T) {
	b, s := allocationServer(t)
	b.pools["acc_1"] = []hme.Alias{fixtureAlias("inactive@example.test", false)}
	code, _, response := allocationCall(s, "/api/v1/aliases", `{"auto_create":false}`)
	if code != 409 || !strings.Contains(response, "NO_AVAILABLE_ALIAS") {
		t.Fatalf("%d %s", code, response)
	}
	code, _, response = allocationCall(s, "/api/v1/aliases", `{"max_aliases":1}`)
	if code != 409 || !strings.Contains(response, "ALIAS_LIMIT_REACHED") || b.creates != 0 {
		t.Fatalf("%d %s", code, response)
	}
}
func TestAllocationPersistentClaimsAndIdempotentReplay(t *testing.T) {
	b, s := allocationServer(t)
	body := `{"account_id":"acc_1","request_id":"fixture-request"}`
	code, first, response := allocationCall(s, "/api/v1/aliases", body)
	if code != 200 {
		t.Fatalf("%d %s", code, response)
	}
	loaded, err := loadAliasUsage(s.cfg.AliasUsageFile)
	if err != nil {
		t.Fatal(err)
	}
	restarted := newWithBackend(b, s.cfg)
	restarted.aliasUsage = loaded
	code, replayed, response := allocationCall(restarted, "/api/v1/aliases", body)
	if code != 200 || replayed != first || b.creates != 0 {
		t.Fatalf("replay: %d %s", code, response)
	}
	code, _, response = allocationCall(restarted, "/api/v1/aliases", `{"request_id":"fixture-request","auto_create":false}`)
	if code != 409 || !strings.Contains(response, "REQUEST_ID_CONFLICT") {
		t.Fatalf("conflict: %d %s", code, response)
	}
	code, next, response := allocationCall(restarted, "/api/v1/aliases", `{"account_id":"acc_1"}`)
	if code != 200 || next.Email == first.Email || next.Source != "created" {
		t.Fatalf("restart duplicate: %d %s", code, response)
	}
	info, err := os.Stat(s.cfg.AliasUsageFile)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("ledger permissions")
	}
}
func TestAllocationConcurrentConsumersDoNotDuplicate(t *testing.T) {
	b, s := allocationServer(t)
	b.pools["acc_1"] = []hme.Alias{fixtureAlias("one@example.test", true), fixtureAlias("two@example.test", true)}
	var wg sync.WaitGroup
	results := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, result, _ := allocationCall(s, "/api/v1/aliases", `{}`)
			if code != 200 {
				results <- "ERROR"
			} else {
				results <- result.Email
			}
		}()
	}
	wg.Wait()
	close(results)
	seen := map[string]bool{}
	for email := range results {
		if email == "ERROR" || seen[email] {
			t.Fatalf("duplicate/error %s", email)
		}
		seen[email] = true
	}
	if len(seen) != 8 || b.creates != 6 || len(s.aliasUsage.claims) != 8 {
		t.Fatal("concurrent allocation count mismatch")
	}
}
func TestAllocationConcurrentSameRequestConsumesOnce(t *testing.T) {
	b, s := allocationServer(t)
	var wg sync.WaitGroup
	results := make(chan string, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, result, _ := allocationCall(s, "/api/v1/aliases", `{"request_id":"one-request"}`)
			if code != 200 {
				results <- "ERROR"
			} else {
				results <- result.Email
			}
		}()
	}
	wg.Wait()
	close(results)
	for email := range results {
		if email != "existing@example.test" {
			t.Fatalf("unexpected %s", email)
		}
	}
	if b.creates != 0 || len(s.aliasUsage.claims) != 1 {
		t.Fatal("idempotency consumed more than once")
	}
}
func TestAllocationImportPreservesHistoricalAndFailedClaims(t *testing.T) {
	b, s := allocationServer(t)
	body := `{"entries":[{"account_id":"acc_1","email":"EXISTING@example.test","claimed_at":123}]}`
	for i := 0; i < 2; i++ {
		code, _, response := allocationCall(s, "/api/v1/aliases/usage", body)
		if code != 200 {
			t.Fatalf("%d %s", code, response)
		}
	}
	loaded, err := loadAliasUsage(s.cfg.AliasUsageFile)
	if err != nil {
		t.Fatal(err)
	}
	s.aliasUsage = loaded
	code, _, response := allocationCall(s, "/api/v1/aliases", `{"auto_create":false}`)
	if code != 409 || !strings.Contains(response, "NO_AVAILABLE_ALIAS") || b.creates != 0 {
		t.Fatalf("%d %s", code, response)
	}
	if s.aliasUsage.claims["existing@example.test"].AllocatedAt != 123 {
		t.Fatal("historical claim overwritten")
	}
}
func TestAllocationErrorsNeverCreateOrRetryBlindly(t *testing.T) {
	b, s := allocationServer(t)
	b.listErrors["acc_1"] = &BackendError{Status: 502, Code: "UPSTREAM_FAILURE", Message: "fixture"}
	code, _, _ := allocationCall(s, "/api/v1/aliases", `{}`)
	if code != 502 || b.creates != 0 {
		t.Fatal("list error treated as empty pool")
	}
	delete(b.listErrors, "acc_1")
	b.pools["acc_1"] = nil
	b.createErr = &BackendError{Status: 502, Code: "UPSTREAM_FAILURE", Message: "fixture"}
	code, _, _ = allocationCall(s, "/api/v1/aliases", `{}`)
	if code != 502 || b.creates != 1 {
		t.Fatal("creation failure retried")
	}
}
func TestAllocationSaveFailureAndCorruptLedgerFailClosed(t *testing.T) {
	b, s := allocationServer(t)
	s.aliasUsage.path = t.TempDir() // rename over a directory fails
	code, _, response := allocationCall(s, "/api/v1/aliases", `{}`)
	if code != 500 || !strings.Contains(response, "ALIAS_USAGE_WRITE_FAILED") || len(s.aliasUsage.claims) != 0 || b.creates != 0 {
		t.Fatalf("%d %s", code, response)
	}
	path := filepath.Join(t.TempDir(), "corrupt.json")
	_ = os.WriteFile(path, []byte(`{"version":1,"claims":{"broken":{}}}`), 0600)
	if _, err := loadAliasUsage(path); err == nil {
		t.Fatal("corrupt ledger silently cleared")
	}
}
func TestAllocationRejectsCreatedPrimary(t *testing.T) {
	b, s := allocationServer(t)
	b.pools["acc_1"] = nil
	b.forceEmail = "owner@icloud.com"
	code, _, response := allocationCall(s, "/api/v1/aliases", `{}`)
	if code != 502 || len(s.aliasUsage.claims) != 0 {
		t.Fatalf("%d %s", code, response)
	}
}
func TestAllocationValidationAndAuthentication(t *testing.T) {
	_, s := allocationServer(t)
	for _, body := range []string{`{"max_aliases":0}`, `{"max_aliases":751}`, `{"max_aliases":1.5}`, `{"auto_create":"false"}`, `{"exclude_emails":["bad"]}`, `{"request_id":"bad id"}`, `{"request_id":"bad/id"}`, `{"label_prefix":"` + strings.Repeat("中", 151) + `"}`} {
		code, _, response := allocationCall(s, "/api/v1/aliases", body)
		if code != 400 {
			t.Fatalf("%d %s", code, response)
		}
	}
	for _, body := range []string{`{}`, `{"entries":null}`, `{"entries":[{"email":"bad"}]}`, `{"entries":[{"email":"valid@example.test","claimed_at":-1}]}`} {
		code, _, response := allocationCall(s, "/api/v1/aliases/usage", body)
		if code != 400 {
			t.Fatalf("%d %s", code, response)
		}
	}
	for _, path := range []string{"/api/v1/aliases", "/api/v1/aliases/allocate", "/api/v1/aliases/usage"} {
		req := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Fatal("allocation/import lacks bearer authentication")
		}
	}
	req := httptest.NewRequest("GET", "/api/v1/aliases/usage", nil)
	req.Header.Set("Authorization", "Bearer "+serviceTestToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"used_count":0`) {
		t.Fatalf("%s", rec.Body.String())
	}
}
func TestAllocationCancellationDoesNotConsume(t *testing.T) {
	_, s := allocationServer(t)
	var req allocateAliasRequest
	_ = normalizeAllocationRequest(&req)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.allocateAlias(ctx, req); !errors.Is(err, context.Canceled) || len(s.aliasUsage.claims) != 0 {
		t.Fatal("cancelled request consumed an alias")
	}
}
