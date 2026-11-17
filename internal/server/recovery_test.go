package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
)

type recoveryFake struct {
	fakeBackend
	mu                       sync.Mutex
	bindings                 map[string]account.RecoveryBinding
	verifyCalls, createCalls int
	verifyErr, errorCreate   error
	verifyHook               func()
	result                   *hme.CreateResult
}

func (b *recoveryFake) RecoveryBinding(id string) (account.RecoveryBinding, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, ok := b.bindings[id]
	return v, ok
}
func (b *recoveryFake) VerifyRecovery(ctx context.Context, v account.RecoveryBinding) error {
	b.mu.Lock()
	b.verifyCalls++
	err, hook := b.verifyErr, b.verifyHook
	b.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}
func (b *recoveryFake) CreateAliasChannel(context.Context, string, string, string) (*hme.CreateResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.createCalls++
	return b.result, b.errorCreate
}
func recoveryFixture(t *testing.T) (*Server, *recoveryFake, *time.Time) {
	t.Helper()
	b := &recoveryFake{bindings: map[string]account.RecoveryBinding{}}
	for _, id := range []string{"one", "two"} {
		b.accounts = append(b.accounts, account.Summary{ID: id, HasCookies: true, HasAppleSession: true})
		b.bindings[id] = account.RecoveryBinding{AccountID: id, SourceID: account.ImportSourceID(id + "@example.test"), Binding: strings.Repeat("a", 64), Session: "old"}
	}
	s := newWithBackend(b, Config{AdminPassword: "fixture-admin-password", ImportToken: testImportToken, APIToken: serviceTestToken, RecoveryFile: filepath.Join(t.TempDir(), "recovery.json")})
	t.Cleanup(s.Close)
	s.recovery.waitDuration = 10 * time.Millisecond
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	s.recovery.now = func() time.Time { return now }
	return s, b, &now
}
func recoveryCall(s *Server, path string, body any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", "/api/integrations/appleservices/recovery/"+path, bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+testImportToken)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func waitBody(b *recoveryFake, id string) recoveryWaitRequest {
	v, _ := b.RecoveryBinding(id)
	return recoveryWaitRequest{Version: 1, ClientID: strings.Repeat("b", 32), Sources: []string{v.SourceID}}
}
func receiveJob(t *testing.T, s *Server, b *recoveryFake, id string) *recoveryJob {
	t.Helper()
	w := recoveryCall(s, "wait", waitBody(b, id))
	var out struct {
		Data struct {
			Job *recoveryJob `json:"job"`
		} `json:"data"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Data.Job == nil {
		t.Fatalf("no job: %d %s", w.Code, w.Body.String())
	}
	return out.Data.Job
}
func completeBody(job *recoveryJob, outcome string) recoveryCompleteRequest {
	return recoveryCompleteRequest{1, strings.Repeat("b", 32), job.ID, job.Lease, outcome}
}
func trigger(s *Server, b *recoveryFake, id string) {
	v, _ := b.RecoveryBinding(id)
	s.requestRecovery(v)
}
func TestRecoveryOnlyCreationLoginRequired(t *testing.T) {
	s, b, _ := recoveryFixture(t)
	for i := 0; i < 7; i++ {
		w := recoveryCall(s, "wait", waitBody(b, "one"))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"job":null`) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if len(s.recovery.records) != 0 || b.verifyCalls != 0 {
		t.Fatal("poll contacted Apple or queued work")
	}
	for _, code := range []string{"APPLE_ACCOUNT_ACTION_REQUIRED", "UPSTREAM_RATE_LIMITED", "ALIAS_LIMIT_REACHED", "UPSTREAM_FAILURE", "ALIAS_CONFIRMATION_REQUIRED"} {
		b.errorCreate = &BackendError{Code: code}
		_, _ = s.createOnChannel(context.Background(), "one", "test", channelApple)
	}
	if len(s.recovery.records) != 0 {
		t.Fatal("non-login error enqueued")
	}
	b.errorCreate = &BackendError{Code: "APPLE_ACCOUNT_LOGIN_REQUIRED"}
	for i := 0; i < 3; i++ {
		_, _ = s.createOnChannel(context.Background(), "one", "test", channelApple)
	}
	if len(s.recovery.records) != 1 {
		t.Fatal("expected exactly one job")
	}
	b.result = &hme.CreateResult{Email: "possibly-created@example.test"}
	_, err := s.createOnChannel(context.Background(), "two", "test", channelApple)
	if asBackendError(err).Code != "ALIAS_CONFIRMATION_REQUIRED" || len(s.recovery.records) != 1 {
		t.Fatal("ambiguous creation triggered recovery")
	}
	job := receiveJob(t, s, b, "one")
	if job.Kind != "apple_login" {
		t.Fatal(job)
	}
	w := recoveryCall(s, "wait", waitBody(b, "two"))
	if !strings.Contains(w.Body.String(), `"job":null`) {
		t.Fatal("cross account lease")
	}
}
func TestRecoveryPreflightMissingSessionAndStaleFailure(t *testing.T) {
	s, b, _ := recoveryFixture(t)
	b.accounts[0].HasAppleSession = false
	b.bindings["one"] = account.RecoveryBinding{AccountID: "one", SourceID: account.ImportSourceID("one@example.test"), Binding: strings.Repeat("a", 64), Session: "false"}
	channels, err := s.creationChannels("one", channelAuto)
	if err != nil || len(channels) != 1 || len(s.recovery.records) != 1 {
		t.Fatal("missing preflight demand")
	}
	old := b.bindings["two"]
	fresh := old
	fresh.Session = "new"
	b.bindings["two"] = fresh
	s.requestRecovery(old)
	if len(s.recovery.records) != 1 {
		t.Fatal("stale failure queued new login")
	}
}
func TestRecoveryConcurrentLeaseAndRestart(t *testing.T) {
	s, b, now := recoveryFixture(t)
	trigger(s, b, "one")
	var wg sync.WaitGroup
	var mu sync.Mutex
	var jobs []*recoveryJob
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, _, _, err := s.claimRecovery(strings.Repeat("b", 32), waitBody(b, "one").Sources)
			if err != nil {
				t.Error(err)
			}
			if j != nil {
				mu.Lock()
				jobs = append(jobs, j)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(jobs) != 1 {
		t.Fatal("duplicate lease", len(jobs))
	}
	original := jobs[0]
	restarted := newRecoveryQueue(s.cfg.RecoveryFile)
	restarted.now = s.recovery.now
	restarted.waitDuration = time.Millisecond
	if err := restarted.load(); err != nil {
		t.Fatal(err)
	}
	s.recovery = restarted
	j, _, _, _ := s.claimRecovery(strings.Repeat("b", 32), waitBody(b, "one").Sources)
	if j != nil {
		t.Fatal("restart lost lease")
	}
	*now = now.Add(301 * time.Second)
	job := receiveJob(t, s, b, "one")
	if job.ID != original.ID || job.Lease == original.Lease {
		t.Fatal("lost-response job id changed")
	}
	if w := recoveryCall(s, "complete", completeBody(original, "failed")); w.Code != 409 {
		t.Fatal("old lease accepted")
	}
	info, _ := os.Stat(s.cfg.RecoveryFile)
	if info.Mode().Perm() != 0600 {
		t.Fatal("queue permissions")
	}
}
func TestRecoveryCompleteReadyRequiresVerificationAndIsIdempotent(t *testing.T) {
	s, b, _ := recoveryFixture(t)
	trigger(s, b, "one")
	job := receiveJob(t, s, b, "one")
	b.verifyErr = errors.New("password-OTP-cookie-secret")
	w := recoveryCall(s, "complete", completeBody(job, "ready"))
	if w.Code != 409 || strings.Contains(w.Body.String(), "secret") {
		t.Fatal(w.Body.String())
	}
	b.verifyErr = nil
	old := b.bindings["one"]
	current := old
	current.Session = "new"
	b.bindings["one"] = current
	// Only the existing integration endpoint can accept a session transition for
	// this lease; a manual change otherwise invalidates it.
	if w = recoveryCall(s, "complete", completeBody(job, "ready")); w.Code != 409 {
		t.Fatal("manual new session did not invalidate old lease")
	}
	s.acceptRecoverySession(old.SourceID)
	for i := 0; i < 2; i++ {
		if w = recoveryCall(s, "complete", completeBody(job, "ready")); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if b.verifyCalls != 2 || b.createCalls != 0 || s.recovery.records["one"].State != "done" {
		t.Fatal("ready replay contacted Apple or created an alias")
	}
}
func TestRecoveryReadyRaceDoesNotBlessNewIdentity(t *testing.T) {
	s, b, _ := recoveryFixture(t)
	trigger(s, b, "one")
	job := receiveJob(t, s, b, "one")
	b.verifyHook = func() {
		b.mu.Lock()
		v := b.bindings["one"]
		v.Binding = strings.Repeat("c", 64)
		b.bindings["one"] = v
		b.mu.Unlock()
	}
	if w := recoveryCall(s, "complete", completeBody(job, "ready")); w.Code != 409 {
		t.Fatal("stale readonly result accepted")
	}
	if s.recovery.records["one"].State == "done" {
		t.Fatal("new identity overwritten")
	}
}
func TestRecoveryPauseOfflineAndBoundedRetries(t *testing.T) {
	s, b, now := recoveryFixture(t)
	trigger(s, b, "one")
	job := receiveJob(t, s, b, "one")
	if w := recoveryCall(s, "complete", completeBody(job, "local_unavailable")); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	trigger(s, b, "one")
	if s.recovery.records["one"].ID != job.ID {
		t.Fatal("offline replaced job")
	}
	*now = now.Add(301 * time.Second)
	retry := receiveJob(t, s, b, "one")
	if retry.ID != job.ID {
		t.Fatal("offline delivery changed id")
	}
	if w := recoveryCall(s, "complete", completeBody(retry, "verification_required")); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	*now = now.Add(24 * time.Hour)
	trigger(s, b, "one")
	w := recoveryCall(s, "wait", waitBody(b, "one"))
	if !strings.Contains(w.Body.String(), `"job":null`) {
		t.Fatal("OTP pause bypassed")
	}
	trigger(s, b, "two")
	for i := 0; i < 3; i++ {
		j := receiveJob(t, s, b, "two")
		for n := 0; n < 2; n++ {
			if w := recoveryCall(s, "complete", completeBody(j, "failed")); w.Code != 200 {
				t.Fatal(w.Body.String())
			}
		}
		*now = now.Add(time.Hour)
		trigger(s, b, "two")
	}
	if s.recovery.records["two"].Failures != 3 || s.recovery.records["two"].State != "exhausted" {
		t.Fatal("unbounded retries")
	}
	w = recoveryCall(s, "wait", waitBody(b, "two"))
	if !strings.Contains(w.Body.String(), `"job":null`) {
		t.Fatal("exhaustion bypassed")
	}
}
func TestRecoveryCancellationAndIndependentWaitSlots(t *testing.T) {
	s, b, _ := recoveryFixture(t)
	s.recovery.waitDuration = time.Second
	// Full authentication semaphore must not block long polling.
	s.importSlots <- struct{}{}
	s.importSlots <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	raw, _ := json.Marshal(waitBody(b, "one"))
	req := httptest.NewRequest("POST", "/api/integrations/appleservices/recovery/wait", bytes.NewReader(raw)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+testImportToken)
	req.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() { defer close(done); s.Handler().ServeHTTP(httptest.NewRecorder(), req) }()
	deadline := time.Now().Add(time.Second)
	for len(s.recovery.waitSlots) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("wait cancellation leaked")
	}
	if len(s.recovery.waitSlots) != 0 || b.verifyCalls != 0 {
		t.Fatal("wait used auth/Apple resources")
	}
	<-s.importSlots
	<-s.importSlots
}
func TestRecoveryStrictProtocolAndLogRedaction(t *testing.T) {
	s, b, _ := recoveryFixture(t)
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	raw, _ := json.Marshal(waitBody(b, "one"))
	base := string(raw)
	bodies := []string{`{}`, strings.Replace(base, `"version":1`, `"version":1,"password":"secret"`, 1), strings.Replace(base, `"version":1`, `"version":1,"version":1`, 1), strings.Replace(base, `"version":1`, `"version":null`, 1), base + `{}`, strings.Replace(base, b.bindings["one"].SourceID, account.ImportSourceID("fake@example.test"), 1)}
	for _, body := range bodies {
		r := httptest.NewRequest("POST", "/api/integrations/appleservices/recovery/wait", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+testImportToken)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code == 200 {
			t.Fatal("invalid request accepted", body)
		}
	}
	for _, token := range []string{"", serviceTestToken, "forged"} {
		r := httptest.NewRequest("POST", "/api/integrations/appleservices/recovery/wait", strings.NewReader(base))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal("wrong auth accepted")
		}
	}
	for _, variant := range []string{"query", "gzip", "oversize"} {
		body := base
		url := "/api/integrations/appleservices/recovery/wait"
		if variant == "query" {
			url += "?TOKEN=secret"
		}
		if variant == "oversize" {
			body = strings.Repeat(" ", 8193)
		}
		r := httptest.NewRequest("POST", url, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+testImportToken)
		r.Header.Set("Content-Type", "application/json")
		if variant == "gzip" {
			r.Header.Set("Content-Encoding", "gzip")
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatal(variant, w.Code)
		}
	}
	if strings.Contains(logs.String(), "secret") || strings.Contains(logs.String(), testImportToken) || strings.Contains(logs.String(), b.bindings["one"].SourceID) {
		t.Fatal("unsafe logs")
	}
}
func TestRecoveryLeaseForgeryAndStorageFailClosed(t *testing.T) {
	s, b, now := recoveryFixture(t)
	trigger(s, b, "one")
	job := receiveJob(t, s, b, "one")
	for _, field := range []string{"client", "lease", "job"} {
		req := completeBody(job, "failed")
		switch field {
		case "client":
			req.ClientID = strings.Repeat("c", 32)
		case "lease":
			req.Lease = strings.Repeat("c", 64)
		case "job":
			req.JobID = "job_unknown"
		}
		if w := recoveryCall(s, "complete", req); w.Code != 409 {
			t.Fatal("forgery accepted")
		}
	}
	*now = now.Add(301 * time.Second)
	if w := recoveryCall(s, "complete", completeBody(job, "failed")); w.Code != 409 {
		t.Fatal("expired lease accepted")
	}
	s.recovery.path = t.TempDir()
	trigger(s, b, "two")
	if _, exists := s.recovery.records["two"]; exists {
		t.Fatal("uncommitted demand visible")
	}
}
func TestRecoveryReadyPreservesStoppedAndRunningSchedules(t *testing.T) {
	s, b, _ := recoveryFixture(t)
	trigger(s, b, "one")
	job := receiveJob(t, s, b, "one")
	next := time.Now().Add(time.Hour)
	schedule := &aliasBatchJob{cancel: func() {}, state: aliasBatchStatus{ID: "batch", AccountID: "one", Status: "stopped", NextRunAt: &next}, dual: &dualScheduleRecord{AppleRejected: true, AppleRevision: "old", Slot: 4, Plan: []time.Time{next}}}
	s.aliasBatches.jobs["one"] = schedule
	if w := recoveryCall(s, "complete", completeBody(job, "ready")); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if schedule.state.Status != "stopped" || !schedule.dual.AppleRejected {
		t.Fatal("stopped task restarted")
	}
	schedule.state.Status = "running"
	s.applyRecoveryReady(s.recovery.records["one"])
	if schedule.dual.AppleRejected || schedule.dual.Slot != 4 || !schedule.state.NextRunAt.Equal(next) || b.createCalls != 0 {
		t.Fatal("ready changed schedule or replayed creation")
	}
}

func TestRecoveryDualCreationEventAndNextNormalSlot(t *testing.T) {
	s, b, now := recoveryFixture(t)
	binding := b.bindings["one"]
	binding.Session = "true"
	b.bindings["one"] = binding
	s.aliasBatches.dualNow = func() time.Time { return *now }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job := &aliasBatchJob{state: aliasBatchStatus{ID: "batch-recovery", AccountID: "one", Channel: channelAuto, Status: "running", Continuous: true}, cancel: cancel}
	s.aliasBatches.mu.Lock()
	s.initializeDualLocked(job, aliasBatchRequest{AccountID: "one", Channel: channelAuto, Continuous: true})
	s.aliasBatches.jobs["one"] = job
	s.aliasBatches.mu.Unlock()
	b.errorCreate = &BackendError{Code: "APPLE_ACCOUNT_LOGIN_REQUIRED", Status: 401}
	*now = job.dual.Plan[0]
	_, done := s.processDualSlot(ctx, job)
	if done || len(s.recovery.records) != 1 || !job.dual.AppleRejected || job.state.Status != "running" {
		t.Fatal("creation demand did not preserve task")
	}
	recoveryJob := receiveJob(t, s, b, "one")
	next := *job.state.NextRunAt
	attempts := len(job.dual.Attempts)
	if w := recoveryCall(s, "complete", completeBody(recoveryJob, "ready")); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if b.createCalls != 1 || job.state.Created != 0 || !job.state.NextRunAt.Equal(next) || len(job.dual.Attempts) != attempts {
		t.Fatal("recovery replayed creation or changed budget")
	}
	_, _ = s.processDualSlot(ctx, job)
	if b.createCalls != 1 {
		t.Fatal("ran before next normal slot")
	}
	b.errorCreate = nil
	b.result = &hme.CreateResult{Email: "next-slot@example.test"}
	*now = next
	_, done = s.processDualSlot(ctx, job)
	if done || b.createCalls != 2 || job.state.Created != 1 {
		t.Fatal("next normal slot did not resume")
	}
}
func TestRecoveryVerificationDoesNotHoldQueueLock(t *testing.T) {
	s, b, _ := recoveryFixture(t)
	trigger(s, b, "one")
	trigger(s, b, "two")
	job := receiveJob(t, s, b, "one")
	started, release := make(chan struct{}), make(chan struct{})
	b.verifyHook = func() { close(started); <-release }
	done := make(chan struct{})
	go func() { defer close(done); recoveryCall(s, "complete", completeBody(job, "ready")) }()
	<-started
	claimDone := make(chan struct{})
	go func() {
		defer close(claimDone)
		_, _, _, _ = s.claimRecovery(strings.Repeat("b", 32), waitBody(b, "two").Sources)
	}()
	select {
	case <-claimDone:
	case <-time.After(time.Second):
		close(release)
		<-done
		t.Fatal("verification held global queue lock")
	}
	close(release)
	<-done
}
