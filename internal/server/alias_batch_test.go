package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
)

// Embed the existing fake for unrelated operations, but synchronize the
// operations called by workers. No tests contact iCloud or register real aliases.
type batchTestBackend struct {
	fakeBackend
	mu                sync.Mutex
	items             []hme.Alias
	labels            []string
	createdAt         []time.Time
	createErr         error
	createErrors      []error
	attempts          int
	attemptedLabels   []string
	commitBeforeError bool
	listErr           error
	createCalled      chan struct{}
	createResume      chan struct{}
}

func (b *batchTestBackend) ListAliases(id string) ([]hme.Alias, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]hme.Alias(nil), b.items...), b.listErr
}

func (b *batchTestBackend) CreateAlias(id, label string) (*hme.CreateResult, error) {
	if b.createCalled != nil {
		b.createCalled <- struct{}{}
		<-b.createResume
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.attempts++
	b.attemptedLabels = append(b.attemptedLabels, label)
	createErr := b.createErr
	if len(b.createErrors) > 0 {
		createErr = b.createErrors[0]
		b.createErrors = b.createErrors[1:]
	}
	if createErr != nil && !b.commitBeforeError {
		return nil, createErr
	}
	n := len(b.items) + 1
	email := fmt.Sprintf("alias-%d@example.test", n)
	b.items = append(b.items, hme.Alias{Email: email, AnonymousID: fmt.Sprint(n), Label: label, Active: true})
	b.labels = append(b.labels, label)
	b.createdAt = append(b.createdAt, time.Now())
	if createErr != nil {
		return nil, createErr
	}
	return &hme.CreateResult{Email: email, Label: label}, nil
}

func (b *batchTestBackend) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.items)
}

func batchTestServer(t *testing.T, total int) (*batchTestBackend, *Server, func(string, string, string) (int, string)) {
	t.Helper()
	b := &batchTestBackend{
		fakeBackend: fakeBackend{accounts: []account.Summary{{ID: "acc_1"}}},
		items:       make([]hme.Alias, total),
	}
	s := newWithBackend(b, Config{AdminPassword: "admin-pass-2026-strong", SessionTTL: 12 * time.Hour})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() { s.Close(); ts.Close() })
	cookie, csrf := login(t, ts, "admin-pass-2026-strong")
	return b, s, func(method, path, body string) (int, string) {
		t.Helper()
		req := authedReq(t, ts, method, path, body)
		req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
		req.Header.Set("X-CSRF-Token", csrf)
		status, response, _ := do(t, req)
		return status, response
	}
}

func batchState(t *testing.T, s *Server) aliasBatchStatus {
	t.Helper()
	s.aliasBatches.mu.Lock()
	defer s.aliasBatches.mu.Unlock()
	job := s.aliasBatches.jobs["acc_1"]
	if job == nil {
		t.Fatal("expected a batch")
	}
	return job.state
}

func waitBatch(t *testing.T, s *Server, predicate func(aliasBatchStatus) bool) aliasBatchStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state := batchState(t, s)
		if predicate(state) {
			return state
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("batch did not reach expected state: %+v", batchState(t, s))
	return aliasBatchStatus{}
}

func TestAliasBatchValidation(t *testing.T) {
	_, _, call := batchTestServer(t, 0)
	for _, body := range []string{
		`{}`, `{"account_id":"acc_1","count":0,"interval_seconds":1}`,
		`{"account_id":"acc_1","count":1,"interval_seconds":1,"cooldown_minutes":0}`,
		`{"account_id":"acc_1","count":1,"interval_seconds":1,"cooldown_minutes":-1}`,
		`{"account_id":"acc_1","count":1,"interval_seconds":1,"cooldown_minutes":1441}`,
		`{"account_id":"acc_1","count":1,"interval_seconds":1,"cooldown_minutes":1.5}`,
		`{"account_id":"acc_1","count":1,"interval_seconds":1,"cooldown_minutes":"60"}`,
		`{"account_id":"acc_1","count":751,"interval_seconds":1}`,
		`{"account_id":"acc_1","count":1.5,"interval_seconds":1}`,
		`{"account_id":"acc_1","count":1,"interval_seconds":0}`,
		`{"account_id":"acc_1","count":1,"interval_seconds":86401}`,
		fmt.Sprintf(`{"account_id":"acc_1","count":1,"interval_seconds":1,"label_prefix":"%s"}`, strings.Repeat("中", 171)),
	} {
		status, response := call("POST", "/api/alias-batch", body)
		if status != 400 || !strings.Contains(response, "VALIDATION_ERROR") {
			t.Fatalf("invalid request accepted: %s -> %d %s", body, status, response)
		}
	}
	status, _ := call("POST", "/api/alias-batch", `{"account_id":"missing","count":1,"interval_seconds":1}`)
	if status != 404 {
		t.Fatalf("expected missing account 404, got %d", status)
	}
	status, response := call("GET", "/api/alias-batch?account_id=acc_1", "")
	if status != 200 || !strings.Contains(response, `"data":null`) {
		t.Fatalf("expected empty job: %d %s", status, response)
	}
}

func TestAliasBatchForbiddenStopsWithoutAutomaticRetry(t *testing.T) {
	b, s, call := batchTestServer(t, 410)
	b.createErr = classifyUpstreamErr("创建邮箱失败", &hme.UpstreamError{Status: 403, Operation: "generate"})
	status, response := call("POST", "/api/alias-batch", `{"account_id":"acc_1","count":10,"interval_seconds":10,"cooldown_minutes":60}`)
	if status != 202 {
		t.Fatalf("start: %d %s", status, response)
	}
	state := waitBatch(t, s, func(state aliasBatchStatus) bool { return state.Status == "failed" })
	if state.ErrorCode != "UPSTREAM_FORBIDDEN" || state.Created != 0 || state.NextRunAt != nil || state.CooldownCount != 0 {
		t.Fatalf("unexpected forbidden state: %+v", state)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.attempts != 1 {
		t.Fatalf("forbidden retried: %d", b.attempts)
	}
}

func TestAliasBatchCompletesWithIntervalAndLabels(t *testing.T) {
	b, s, call := batchTestServer(t, 0)
	status, response := call("POST", "/api/alias-batch", `{"account_id":"acc_1","count":2,"interval_seconds":1,"label_prefix":"批次"}`)
	if status != 202 {
		t.Fatalf("start: %d %s", status, response)
	}
	state := waitBatch(t, s, func(state aliasBatchStatus) bool { return state.Status == "completed" })
	if state.Created != 2 || state.AliasTotal != 2 || state.FinishedAt == nil || state.NextRunAt != nil {
		t.Fatalf("unexpected final state: %+v", state)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.labels) != 2 || !strings.HasSuffix(b.labels[0], "-001") || !strings.HasSuffix(b.labels[1], "-002") {
		t.Fatalf("labels: %v", b.labels)
	}
	if b.createdAt[1].Sub(b.createdAt[0]) < time.Second {
		t.Fatal("configured interval was not observed")
	}
}

func TestAliasBatchQuotaAndAllCreationEntryPoints(t *testing.T) {
	b, s, call := batchTestServer(t, 749)
	status, response := call("POST", "/api/alias-batch", `{"account_id":"acc_1","count":2,"interval_seconds":1}`)
	if status != 400 || !strings.Contains(response, "ALIAS_QUOTA_EXCEEDED") {
		t.Fatalf("quota: %d %s", status, response)
	}
	status, response = call("POST", "/api/alias-batch", `{"account_id":"acc_1","count":1,"interval_seconds":1}`)
	if status != 202 {
		t.Fatalf("start: %d %s", status, response)
	}
	waitBatch(t, s, func(state aliasBatchStatus) bool { return state.Status == "completed" })
	if b.count() != 750 {
		t.Fatalf("count = %d", b.count())
	}
	for _, path := range []string{"/api/create", "/api/alias-batch"} {
		status, response := call("POST", path, `{"account_id":"acc_1","count":1,"interval_seconds":1}`)
		if status != 409 || !strings.Contains(response, "ALIAS_LIMIT_REACHED") {
			t.Fatalf("cap not applied to %s: %d %s", path, status, response)
		}
	}
}

func TestConcurrentCreationCannotExceedQuota(t *testing.T) {
	b, s, _ := batchTestServer(t, 749)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := s.createAliasWithinLimit(context.Background(), "acc_1", "manual")
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 || b.count() != 750 {
		t.Fatalf("successes=%d total=%d", successes, b.count())
	}
}

func TestAliasBatchStopAndDuplicateStart(t *testing.T) {
	b, s, call := batchTestServer(t, 0)
	status, response := call("POST", "/api/alias-batch", `{"account_id":"acc_1","count":3,"interval_seconds":86400}`)
	if status != 202 {
		t.Fatalf("start: %d %s", status, response)
	}
	state := waitBatch(t, s, func(state aliasBatchStatus) bool { return state.Created == 1 && state.NextRunAt != nil })
	status, response = call("POST", "/api/alias-batch", `{"account_id":"acc_1","count":1,"interval_seconds":1}`)
	if status != 409 || !strings.Contains(response, "BATCH_ALREADY_RUNNING") {
		t.Fatalf("duplicate: %d %s", status, response)
	}
	status, response = call("POST", "/api/alias-batch/stop", fmt.Sprintf(`{"account_id":"acc_1","job_id":"%s"}`, state.ID))
	if status != 200 {
		t.Fatalf("stop: %d %s", status, response)
	}
	state = waitBatch(t, s, func(state aliasBatchStatus) bool { return state.Status == "stopped" })
	if state.Created != 1 || b.count() != 1 || state.NextRunAt != nil {
		t.Fatalf("stop state: %+v", state)
	}
	status, _ = call("POST", "/api/alias-batch/stop", `{"account_id":"acc_1","job_id":"stale"}`)
	if status != 404 {
		t.Fatalf("stale stop: %d", status)
	}
	status, _ = call("POST", "/api/alias-batch", `{"account_id":"acc_1","count":1,"interval_seconds":1}`)
	if status != 202 {
		t.Fatalf("restart: %d", status)
	}
	waitBatch(t, s, func(state aliasBatchStatus) bool { return state.Status == "completed" })
}

func TestAliasBatchStopDuringInFlightCreation(t *testing.T) {
	b, s, call := batchTestServer(t, 0)
	b.createCalled = make(chan struct{}, 1)
	b.createResume = make(chan struct{})
	status, _ := call("POST", "/api/alias-batch", `{"account_id":"acc_1","count":3,"interval_seconds":1}`)
	if status != 202 {
		t.Fatalf("start: %d", status)
	}
	select {
	case <-b.createCalled:
	case <-time.After(3 * time.Second):
		t.Fatal("create was not called")
	}
	state := batchState(t, s)
	status, _ = call("POST", "/api/alias-batch/stop", fmt.Sprintf(`{"account_id":"acc_1","job_id":"%s"}`, state.ID))
	if status != 200 || batchState(t, s).Status != "stopping" {
		t.Fatalf("stop: %d", status)
	}
	close(b.createResume)
	state = waitBatch(t, s, func(state aliasBatchStatus) bool { return state.Status == "stopped" })
	if state.Created != 1 || b.count() != 1 {
		t.Fatalf("in-flight stop: %+v", state)
	}
}

func TestAliasBatchStopsOnFailureWithoutRetry(t *testing.T) {
	b, s, call := batchTestServer(t, 0)
	b.createErr = &BackendError{Status: 401, Code: "UPSTREAM_UNAUTHORIZED", Message: "请更新 Cookie"}
	status, _ := call("POST", "/api/alias-batch", `{"account_id":"acc_1","count":10,"interval_seconds":1}`)
	if status != 202 {
		t.Fatalf("start: %d", status)
	}
	state := waitBatch(t, s, func(state aliasBatchStatus) bool { return state.Status == "failed" })
	if state.Created != 0 || state.ErrorCode != "UPSTREAM_UNAUTHORIZED" || state.NextRunAt != nil {
		t.Fatalf("failure: %+v", state)
	}
}

func TestAliasBatchRechecksQuotaBetweenCreations(t *testing.T) {
	b, s, call := batchTestServer(t, 748)
	status, _ := call("POST", "/api/alias-batch", `{"account_id":"acc_1","count":2,"interval_seconds":1}`)
	if status != 202 {
		t.Fatalf("start: %d", status)
	}
	waitBatch(t, s, func(state aliasBatchStatus) bool { return state.Created == 1 && state.NextRunAt != nil })
	// Another creation consumes the last slot while the worker waits.
	status, response := call("POST", "/api/create", `{"account_id":"acc_1","label":"manual"}`)
	if status != 200 {
		t.Fatalf("manual create: %d %s", status, response)
	}
	state := waitBatch(t, s, func(state aliasBatchStatus) bool { return state.Status == "limit_reached" })
	if state.Created != 1 || state.AliasTotal != 750 || b.count() != 750 {
		t.Fatalf("quota: %+v", state)
	}
}

func TestAliasBatchSessionAndCSRF(t *testing.T) {
	s, ts := newTestServer(&fakeBackend{})
	defer ts.Close()
	defer s.Close()
	for _, endpoint := range []struct{ method, path, body string }{
		{"GET", "/api/alias-batch?account_id=acc_1", ""},
		{"POST", "/api/alias-batch", `{"account_id":"acc_1","count":1,"interval_seconds":1}`},
		{"POST", "/api/alias-batch/stop", `{"account_id":"acc_1","job_id":"id"}`},
	} {
		status, _, _ := do(t, authedReq(t, ts, endpoint.method, endpoint.path, endpoint.body))
		if status != 401 {
			t.Fatalf("unauthenticated %s: %d", endpoint.path, status)
		}
		if endpoint.method == "POST" {
			cookie, _ := login(t, ts, "admin-pass-2026-strong")
			req := authedReq(t, ts, endpoint.method, endpoint.path, endpoint.body)
			req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
			status, response, _ := do(t, req)
			var out struct {
				Code string `json:"code"`
			}
			_ = json.Unmarshal([]byte(response), &out)
			if status != 403 || out.Code != "CSRF_INVALID" {
				t.Fatalf("CSRF: %d %s", status, response)
			}
		}
	}
}

func TestAliasLimitAlsoAppliesToTokenAPI(t *testing.T) {
	_, s, _ := batchTestServer(t, 750)
	s.apiSettings.mu.Lock()
	s.apiSettings.value.Token = strings.Repeat("x", 32)
	s.apiSettings.value.Enabled = true
	s.apiSettings.mu.Unlock()
	req := httptest.NewRequest("POST", "/api/v1/aliases", strings.NewReader(`{"account_id":"acc_1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("x", 32))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "ALIAS_LIMIT_REACHED") {
		t.Fatalf("token API quota: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAliasBatchCooldownResumesRemainingItems(t *testing.T) {
	b, s, call := batchTestServer(t, 0)
	s.aliasBatches.minuteDuration = 80 * time.Millisecond / defaultCooldownMinutes
	b.createErrors = []error{nil, &BackendError{Status: 502, Code: "UPSTREAM_FAILURE", Message: "临时失败"}, nil}
	status, _ := call("POST", "/api/alias-batch", `{"account_id":"acc_1","count":2,"interval_seconds":1}`)
	if status != 202 {
		t.Fatalf("start: %d", status)
	}
	state := waitBatch(t, s, func(st aliasBatchStatus) bool { return st.Status == "cooling_down" })
	if state.Created != 1 || state.NextRunAt == nil || state.FinishedAt != nil || state.CooldownCount != 1 {
		t.Fatalf("cooldown: %+v", state)
	}
	state = waitBatch(t, s, func(st aliasBatchStatus) bool { return st.Status == "completed" })
	if state.Created != 2 || state.AliasTotal != 2 || state.ErrorCode != "" || state.NextRunAt != nil {
		t.Fatalf("completion: %+v", state)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.attempts != 3 || b.attemptedLabels[1] != b.attemptedLabels[2] {
		t.Fatalf("attempts: %v", b.attemptedLabels)
	}
}

func TestAliasBatchCooldownReconcilesLostResponse(t *testing.T) {
	b, s, call := batchTestServer(t, 0)
	s.aliasBatches.minuteDuration = 30 * time.Millisecond / defaultCooldownMinutes
	b.createErr = &BackendError{Status: 502, Code: "UPSTREAM_FAILURE", Message: "响应丢失"}
	b.commitBeforeError = true
	status, _ := call("POST", "/api/alias-batch", `{"account_id":"acc_1","count":1,"interval_seconds":1}`)
	if status != 202 {
		t.Fatalf("start: %d", status)
	}
	state := waitBatch(t, s, func(st aliasBatchStatus) bool { return st.Status == "completed" })
	b.mu.Lock()
	defer b.mu.Unlock()
	if state.Created != 1 || state.AliasTotal != 1 || b.attempts != 1 || len(b.items) != 1 {
		t.Fatalf("duplicate reservation: state=%+v calls=%d items=%d", state, b.attempts, len(b.items))
	}
}

func TestAliasBatchCooldownCanStopAndBlocksDuplicate(t *testing.T) {
	b, s, call := batchTestServer(t, 0)
	b.createErr = &BackendError{Status: 429, Code: "UPSTREAM_RATE_LIMITED", Message: "频率受限"}
	status, _ := call("POST", "/api/alias-batch", `{"account_id":"acc_1","count":2,"interval_seconds":1}`)
	if status != 202 {
		t.Fatalf("start: %d", status)
	}
	state := waitBatch(t, s, func(st aliasBatchStatus) bool { return st.Status == "cooling_down" })
	if state.NextRunAt == nil || time.Until(*state.NextRunAt) < 59*time.Minute {
		t.Fatalf("default delay: %+v", state)
	}
	status, _ = call("POST", "/api/alias-batch", `{"account_id":"acc_1","count":1,"interval_seconds":1}`)
	if status != 409 {
		t.Fatalf("duplicate start accepted: %d", status)
	}
	status, _ = call("POST", "/api/alias-batch/stop", fmt.Sprintf(`{"account_id":"acc_1","job_id":"%s"}`, state.ID))
	if status != 200 {
		t.Fatalf("stop: %d", status)
	}
	state = waitBatch(t, s, func(st aliasBatchStatus) bool { return st.Status == "stopped" })
	if state.Created != 0 || state.NextRunAt != nil || state.FinishedAt == nil {
		t.Fatalf("stop: %+v", state)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.attempts != 1 {
		t.Fatalf("unexpected retry: %d", b.attempts)
	}
}

func TestAliasBatchRetryAfterAndBackoff(t *testing.T) {
	for _, tc := range []struct {
		failures   int
		hint, want time.Duration
	}{
		{1, 0, time.Hour}, {2, 0, 2 * time.Hour}, {3, 0, 2 * time.Hour}, {100, 0, 2 * time.Hour},
		{1, 10 * time.Minute, time.Hour}, {1, 3 * time.Hour, 3 * time.Hour},
	} {
		if got := cooldownDelay(time.Hour, tc.failures, tc.hint); got != tc.want {
			t.Fatalf("delay=%v want=%v", got, tc.want)
		}
	}
	b, s, call := batchTestServer(t, 0)
	s.aliasBatches.minuteDuration = 10 * time.Millisecond / defaultCooldownMinutes
	b.createErrors = []error{&BackendError{Status: 429, Code: "UPSTREAM_RATE_LIMITED", Message: "受限", RetryAfter: 150 * time.Millisecond}, nil}
	began := time.Now()
	call("POST", "/api/alias-batch", `{"account_id":"acc_1","count":1,"interval_seconds":1}`)
	state := waitBatch(t, s, func(st aliasBatchStatus) bool { return st.Status == "completed" })
	if time.Since(began) < 150*time.Millisecond || state.CooldownCount != 1 {
		t.Fatalf("Retry-After ignored: %+v", state)
	}
}

func TestAliasBatchCooldownDoesNotCreateWhenReconciliationFails(t *testing.T) {
	b, s, call := batchTestServer(t, 0)
	s.aliasBatches.minuteDuration = 100 * time.Millisecond / defaultCooldownMinutes
	b.createErr = &BackendError{Status: 502, Code: "UPSTREAM_FAILURE", Message: "连接失败"}
	call("POST", "/api/alias-batch", `{"account_id":"acc_1","count":1,"interval_seconds":1}`)
	waitBatch(t, s, func(st aliasBatchStatus) bool { return st.Status == "cooling_down" })
	b.mu.Lock()
	b.listErr = &BackendError{Status: 502, Code: "UPSTREAM_FAILURE", Message: "列表失败"}
	b.createErr = nil
	b.mu.Unlock()
	waitBatch(t, s, func(st aliasBatchStatus) bool { return st.CooldownCount == 2 })
	s.Close()
	waitBatch(t, s, func(st aliasBatchStatus) bool { return st.Status == "stopped" })
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.attempts != 1 {
		t.Fatalf("created without authoritative list: %d", b.attempts)
	}
}

// Start the real worker with a millisecond interval; HTTP still enforces >= 1s.
func startFastBatch(t *testing.T, s *Server, count, cooldownMinutes int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req := aliasBatchRequest{AccountID: "acc_1", Count: count, IntervalSeconds: 0.001,
		CooldownMinutes: cooldownMinutes, LabelPrefix: "test"}
	job := &aliasBatchJob{state: aliasBatchStatus{ID: "test-job", AccountID: req.AccountID,
		Status: "running", Count: count, IntervalSeconds: req.IntervalSeconds,
		CooldownMinutes: cooldownMinutes, BatchSize: aliasBatchSize}, cancel: cancel}
	s.aliasBatches.mu.Lock()
	s.aliasBatches.jobs[req.AccountID] = job
	s.aliasBatches.mu.Unlock()
	go s.runAliasBatch(ctx, job, req)
}

func TestAliasBatchFiveAtATimeAndResumesToCompletion(t *testing.T) {
	b, s, _ := batchTestServer(t, 0)
	s.aliasBatches.minuteDuration = 5 * time.Millisecond
	startFastBatch(t, s, 12, 40)
	for _, created := range []int{5, 10} {
		state := waitBatch(t, s, func(st aliasBatchStatus) bool {
			return st.Status == "cooling_down" && st.Created == created
		})
		if state.NextRunAt == nil || state.FinishedAt != nil || state.ErrorCode != "" || state.CooldownMinutes != 40 {
			t.Fatalf("scheduled cooldown: %+v", state)
		}
		time.Sleep(50 * time.Millisecond)
		if got := b.count(); got != created {
			t.Fatalf("created during cooldown: got=%d want=%d", got, created)
		}
	}
	state := waitBatch(t, s, func(st aliasBatchStatus) bool { return st.Status == "completed" })
	if state.Created != 12 || b.count() != 12 || state.CooldownCount != 2 || state.NextRunAt != nil {
		t.Fatalf("completion: %+v", state)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, i := range []int{5, 10} {
		if elapsed := b.createdAt[i].Sub(b.createdAt[i-1]); elapsed < 200*time.Millisecond {
			t.Fatalf("configured cooldown ignored at %d: %v", i, elapsed)
		}
	}
}

func TestAliasBatchNoCooldownAfterFinalItem(t *testing.T) {
	for _, count := range []int{1, 5, 10} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			b, s, _ := batchTestServer(t, 0)
			s.aliasBatches.minuteDuration = time.Millisecond
			startFastBatch(t, s, count, 1)
			state := waitBatch(t, s, func(st aliasBatchStatus) bool { return st.Status == "completed" })
			if state.Created != count || b.count() != count || state.CooldownCount != (count-1)/5 || state.NextRunAt != nil {
				t.Fatalf("unnecessary final cooldown: %+v", state)
			}
		})
	}
}

func TestAliasBatchStopScheduledCooldownNeverResumes(t *testing.T) {
	b, s, call := batchTestServer(t, 0)
	s.aliasBatches.minuteDuration = 5 * time.Millisecond
	startFastBatch(t, s, 12, 40)
	state := waitBatch(t, s, func(st aliasBatchStatus) bool { return st.Status == "cooling_down" })
	status, _ := call("POST", "/api/alias-batch/stop", fmt.Sprintf(`{"account_id":"acc_1","job_id":"%s"}`, state.ID))
	if status != 200 {
		t.Fatalf("stop: %d", status)
	}
	state = waitBatch(t, s, func(st aliasBatchStatus) bool { return st.Status == "stopped" })
	time.Sleep(250 * time.Millisecond)
	if state.Created != 5 || b.count() != 5 || state.NextRunAt != nil || state.FinishedAt == nil {
		t.Fatalf("resumed after stop: %+v, actual=%d", state, b.count())
	}
}

func TestAliasBatchCooldownSettingDefaultsAndCustomValue(t *testing.T) {
	for _, minutes := range []int{0, 1, 45, 1440} {
		t.Run(fmt.Sprint(minutes), func(t *testing.T) {
			_, s, call := batchTestServer(t, 0)
			field := ""
			want := defaultCooldownMinutes
			if minutes > 0 {
				field = fmt.Sprintf(`,"cooldown_minutes":%d`, minutes)
				want = minutes
			}
			status, response := call("POST", "/api/alias-batch", `{"account_id":"acc_1","count":1,"interval_seconds":1`+field+`}`)
			if status != 202 {
				t.Fatalf("start: %d %s", status, response)
			}
			state := waitBatch(t, s, func(st aliasBatchStatus) bool { return st.Status == "completed" })
			if state.CooldownMinutes != want || state.BatchSize != 5 {
				t.Fatalf("setting not retained: %+v", state)
			}
		})
	}
}

func TestAliasBatchCustomErrorCooldownAndLongSetting(t *testing.T) {
	for _, tc := range []struct {
		base             time.Duration
		failures         int
		retryAfter, want time.Duration
	}{
		{15 * time.Minute, 1, 0, 15 * time.Minute},
		{15 * time.Minute, 2, 0, 30 * time.Minute},
		{15 * time.Minute, 10, 0, 2 * time.Hour},
		{3 * time.Hour, 1, 0, 3 * time.Hour},
		{3 * time.Hour, 100, 0, 3 * time.Hour},
		{3 * time.Hour, 1, 4 * time.Hour, 4 * time.Hour},
	} {
		if got := cooldownDelay(tc.base, tc.failures, tc.retryAfter); got != tc.want {
			t.Fatalf("cooldown=%v want=%v", got, tc.want)
		}
	}
	b, s, _ := batchTestServer(t, 0)
	s.aliasBatches.minuteDuration = 5 * time.Millisecond
	b.createErrors = []error{&BackendError{Status: 429, Code: "UPSTREAM_RATE_LIMITED", Message: "频率受限"}, nil}
	startFastBatch(t, s, 1, 40)
	state := waitBatch(t, s, func(st aliasBatchStatus) bool { return st.Status == "cooling_down" })
	if state.Created != 0 || state.NextRunAt == nil || time.Until(*state.NextRunAt) < 100*time.Millisecond {
		t.Fatalf("custom error cooldown ignored: %+v", state)
	}
	state = waitBatch(t, s, func(st aliasBatchStatus) bool { return st.Status == "completed" })
	if state.Created != 1 || b.count() != 1 {
		t.Fatalf("retry not completed: %+v", state)
	}
}
