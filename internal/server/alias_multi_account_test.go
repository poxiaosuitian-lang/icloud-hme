package server

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
)

func multiAllocationServer(t *testing.T, inventory int) (*allocationTestBackend, *Server) {
	t.Helper()
	b, s := allocationServer(t)
	// Deliberately reverse backend order; never rely on map/list iteration order.
	b.accounts = []account.Summary{{ID: "acc_3", Name: "Third"}, {ID: "acc_2", Name: "Second"}, {ID: "acc_1", Name: "First"}}
	for _, acc := range b.accounts {
		b.pools[acc.ID] = nil
		for i := 0; i < inventory; i++ {
			b.pools[acc.ID] = append(b.pools[acc.ID], fixtureAlias(fmt.Sprintf("%s-%d@example.test", acc.ID, i), true))
		}
	}
	return b, s
}

func TestMultiAccountBalancedAcrossRestartAndReplay(t *testing.T) {
	b, s := multiAllocationServer(t, 4)
	var first aliasAllocation
	for i := 0; i < 9; i++ {
		body := fmt.Sprintf(`{"caller_id":"vendor-a","request_id":"request-%d","auto_create":false}`, i)
		code, result, raw := allocationCall(s, "/api/v1/aliases", body)
		if code != 200 || result.AccountID != fmt.Sprintf("acc_%d", i%3+1) {
			t.Fatalf("allocation %d: %d %s", i, code, raw)
		}
		if i == 0 {
			first = result
		}
		if i == 3 {
			loaded, err := loadAliasUsage(s.cfg.AliasUsageFile)
			if err != nil {
				t.Fatal(err)
			}
			s = newWithBackend(b, s.cfg)
			s.aliasUsage = loaded
			code, replay, raw := allocationCall(s, "/api/v1/aliases", `{"caller_id":"vendor-a","request_id":"request-0","auto_create":false}`)
			if code != 200 || replay != first {
				t.Fatalf("replay moved account: %d %s", code, raw)
			}
		}
	}
	if b.creates != 0 || len(s.aliasUsage.claims) != 9 {
		t.Fatal("unexpected creation or ledger count")
	}
}

func TestMultiAccountBalancesCallerThenGlobalLoad(t *testing.T) {
	_, s := multiAllocationServer(t, 4)
	call := func(body, want string) {
		t.Helper()
		code, result, raw := allocationCall(s, "/api/v1/aliases", body)
		if code != 200 || result.AccountID != want {
			t.Fatalf("want %s: %d %s", want, code, raw)
		}
	}
	call(`{"caller_id":"a","account_id":"acc_1"}`, "acc_1")
	call(`{"caller_id":"a","account_id":"acc_1"}`, "acc_1")
	call(`{"caller_id":"b"}`, "acc_2") // Global load breaks this caller's tie.
	call(`{"caller_id":"b"}`, "acc_3")
	call(`{"caller_id":"b"}`, "acc_1") // Per-caller fairness takes priority.
	call(`{"caller_id":"a"}`, "acc_2")
}

func TestMultiAccountUnhealthyDoesNotBlockHealthy(t *testing.T) {
	for _, existing := range []bool{true, false} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			b, s := multiAllocationServer(t, 0)
			b.listErrors["acc_1"] = &BackendError{Status: 401, Code: "UPSTREAM_UNAUTHORIZED", Message: "fixture"}
			if existing {
				b.pools["acc_3"] = []hme.Alias{fixtureAlias("healthy@example.test", true)}
			}
			code, result, raw := allocationCall(s, "/api/v1/aliases", `{}`)
			want := "acc_2"
			if existing {
				want = "acc_3"
			}
			if code != 200 || result.AccountID != want {
				t.Fatalf("%d %s", code, raw)
			}
			if existing && b.creates != 0 {
				t.Fatal("created before scanning healthy inventory")
			}
			if !existing && (b.creates != 1 || b.createdAccount != "acc_2") {
				t.Fatal("wrong creation target")
			}
		})
	}
}

func TestMultiAccountPinnedFailureNeverFallsBack(t *testing.T) {
	b, s := multiAllocationServer(t, 2)
	b.listErrors["acc_1"] = &BackendError{Status: 401, Code: "UPSTREAM_UNAUTHORIZED", Message: "fixture"}
	code, _, raw := allocationCall(s, "/api/v1/aliases", `{"account_id":"acc_1"}`)
	if code != 401 || !strings.Contains(raw, "UPSTREAM_UNAUTHORIZED") || len(s.aliasUsage.claims) != 0 || b.creates != 0 {
		t.Fatalf("%d %s", code, raw)
	}
}

func TestMultiAccountAllUnreadableDoesNotCreate(t *testing.T) {
	b, s := multiAllocationServer(t, 0)
	for _, a := range b.accounts {
		b.listErrors[a.ID] = &BackendError{Status: 502, Code: "UPSTREAM_FAILURE", Message: "fixture"}
	}
	code, _, raw := allocationCall(s, "/api/v1/aliases", `{}`)
	if code != 502 || b.creates != 0 {
		t.Fatalf("%d %s", code, raw)
	}
}

func TestMultiAccountCreationBalancedAndQuotaFallback(t *testing.T) {
	b, s := multiAllocationServer(t, 0)
	// Full first account contains no usable aliases. Capacity is per account.
	b.pools["acc_1"] = []hme.Alias{fixtureAlias("inactive@example.test", false)}
	for _, want := range []string{"acc_2", "acc_3"} {
		code, result, raw := allocationCall(s, "/api/v1/aliases", `{"max_aliases":1}`)
		if code != 200 || result.Source != "created" || result.AccountID != want {
			t.Fatalf("%d %s", code, raw)
		}
	}
	code, _, raw := allocationCall(s, "/api/v1/aliases", `{"max_aliases":1}`)
	if code != 409 || !strings.Contains(raw, "ALIAS_LIMIT_REACHED") || b.creates != 2 {
		t.Fatalf("%d %s", code, raw)
	}
}

func TestMultiAccountAmbiguousCreateNeverTriesAnotherAccount(t *testing.T) {
	b, s := multiAllocationServer(t, 0)
	b.createErr = &BackendError{Status: 502, Code: "UPSTREAM_FAILURE", Message: "fixture timeout after send"}
	code, _, raw := allocationCall(s, "/api/v1/aliases", `{}`)
	if code != 502 || b.creates != 1 || len(s.aliasUsage.claims) != 0 {
		t.Fatalf("%d %s", code, raw)
	}
}

func TestMultiAccountConcurrentBalancedNoDuplicates(t *testing.T) {
	b, s := multiAllocationServer(t, 10)
	results := make(chan aliasAllocation, 24)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, r, _ := allocationCall(s, "/api/v1/aliases", `{"auto_create":false}`)
			if code != 200 {
				r.Email = ""
			}
			results <- r
		}()
	}
	wg.Wait()
	close(results)
	seen, counts := map[string]bool{}, map[string]int{}
	for r := range results {
		if r.Email == "" || seen[r.Email] {
			t.Fatal("duplicate or failed allocation")
		}
		seen[r.Email] = true
		counts[r.AccountID]++
	}
	for _, a := range b.accounts {
		if counts[a.ID] != 8 {
			t.Fatalf("unbalanced: %#v", counts)
		}
	}
	if b.creates != 0 {
		t.Fatal("unnecessary creation")
	}
}

func TestMultiAccountUsageIncludesScopedAndHistoricalGroups(t *testing.T) {
	_, s := multiAllocationServer(t, 2)
	for _, body := range []string{`{"caller_id":"a","account_id":"acc_1"}`, `{"caller_id":"b","account_id":"acc_1"}`} {
		if code, _, raw := allocationCall(s, "/api/v1/aliases", body); code != 200 {
			t.Fatal(raw)
		}
	}
	code, _, raw := allocationCall(s, "/api/v1/aliases/usage", `{"caller_id":"a","entries":[{"account_id":"removed","email":"old@example.test"},{"email":"unassigned@example.test"}]}`)
	if code != 200 {
		t.Fatal(raw)
	}
	all := callerUsage(t, s, "")
	if all["allocation_policy"] != "least_used_existing_first" {
		t.Fatal("missing allocation policy")
	}
	rows := all["accounts"].([]any)
	if len(rows) != 5 {
		t.Fatalf("missing current or historical groups: %#v", rows)
	}
	for _, row := range rows {
		r := row.(map[string]any)
		if r["account_id"] == "acc_1" && (r["used_count"] != float64(2) || r["unique_email_count"] != float64(1)) {
			t.Fatalf("wrong counts: %#v", r)
		}
		if r["account_id"] == "removed" && r["available"] != false {
			t.Fatal("removed account eligible")
		}
	}
	scoped := callerUsage(t, s, "?caller_id=b")
	if len(scoped["accounts"].([]any)) != 3 || scoped["used_count"] != float64(1) {
		t.Fatalf("caller history leaked: %#v", scoped)
	}
}

// Simulate inventory or connection changes between the initial scan and the
// locked pre-creation check. The second directory read must hold the same lock
// used by manual and scheduled creators.
type changingAllocationBackend struct {
	*allocationTestBackend
	calls int
	fail  bool
	lock  *sync.Mutex
	t     *testing.T
}

func (b *changingAllocationBackend) ListAliases(id string) ([]hme.Alias, error) {
	if id == "acc_1" {
		b.calls++
		if b.calls == 2 {
			if b.lock.TryLock() {
				b.lock.Unlock()
				b.t.Error("preflight did not share creation lock")
			}
			if b.fail {
				return nil, &BackendError{Status: 401, Code: "UPSTREAM_UNAUTHORIZED", Message: "fixture"}
			}
			return []hme.Alias{fixtureAlias("background-refill@example.test", true)}, nil
		}
	}
	return b.allocationTestBackend.ListAliases(id)
}
func TestMultiAccountLockedPreflightReusesRefillOrFallsBack(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			b, s := multiAllocationServer(t, 0)
			s.be = &changingAllocationBackend{allocationTestBackend: b, fail: fail, lock: s.aliasBatches.creationLock("acc_1"), t: t}
			code, result, raw := allocationCall(s, "/api/v1/aliases", `{}`)
			if code != 200 {
				t.Fatalf("%d %s", code, raw)
			}
			if fail {
				if result.AccountID != "acc_2" || b.creates != 1 {
					t.Fatal("healthy account not used")
				}
			} else if result.Email != "background-refill@example.test" || result.Source != "existing" || b.creates != 0 {
				t.Fatal("background inventory ignored")
			}
		})
	}
}
