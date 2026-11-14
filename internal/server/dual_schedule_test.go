package server

import (
	"context"
	"encoding/json"
	"fmt"
	"icloud-hme/internal/account"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func dualFixture(t *testing.T, count int, path string) (*channelFake, *Server, *aliasBatchJob, *time.Time, context.Context) {
	t.Helper()
	b := &channelFake{}
	b.accounts = []account.Summary{{ID: "acc_1", HasAppleSession: true, HasCookies: true}}
	s := newWithBackend(b, Config{AdminPassword: "admin-pass-2026-strong", DualScheduleFile: path})
	now := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	s.aliasBatches.dualNow = func() time.Time { return now }
	ctx, cancel := context.WithCancel(context.Background())
	req := aliasBatchRequest{AccountID: "acc_1", Channel: channelAuto, Count: count, LabelPrefix: "fixture"}
	job := &aliasBatchJob{state: aliasBatchStatus{ID: "test", AccountID: "acc_1", Channel: channelAuto, Status: "running", Count: count}, cancel: cancel}
	s.aliasBatches.mu.Lock()
	ok := s.initializeDualLocked(job, req)
	s.aliasBatches.jobs["acc_1"] = job
	s.aliasBatches.mu.Unlock()
	if !ok {
		t.Fatal("initialize")
	}
	t.Cleanup(s.Close)
	return b, s, job, &now, ctx
}
func driveDualSlot(s *Server, job *aliasBatchJob, now *time.Time, ctx context.Context) bool {
	*now = job.dual.Plan[job.dual.Slot]
	_, done := s.processDualSlot(ctx, job)
	return done
}
func TestDualPlanMatchesUpstreamDistributionAndGaps(t *testing.T) {
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for seed := 0; seed < 10; seed++ {
		n := seed
		plan := makeDualPlan(anchor, func(max int) int { n = (n + 7) % max; return n })
		last := anchor
		web := []int{}
		only := 0
		for i, at := range plan {
			if at.Sub(last) < 2*time.Minute {
				t.Fatal("gap")
			}
			last = at
			if dualSlotChannel(i) == channelWeb {
				web = append(web, i)
			}
			if dualWebOnlySlot(i) {
				only++
			}
		}
		if !last.Equal(anchor.Add(time.Hour)) || len(web) != 4 || only != 5 {
			t.Fatal("plan limits")
		}
		for i, want := range []int{5, 11, 17, 22} {
			if web[i] != want {
				t.Fatal(web)
			}
		}
	}
}
func TestDualCreatesNineteenAndFourNotAlternating(t *testing.T) {
	_, s, job, now, ctx := dualFixture(t, 23, "")
	for i := 0; i < 23; i++ {
		done := driveDualSlot(s, job, now, ctx)
		if done != (i == 22) {
			t.Fatal("unexpected finish", i, job.state)
		}
	}
	if job.state.Status != "completed" || job.dual.ChannelCreated[channelApple] != 19 || job.dual.ChannelCreated[channelWeb] != 4 {
		t.Fatal(job.state, job.dual.ChannelCreated)
	}
	if job.state.Channels[1].Created != 4 {
		t.Fatal("stale final channel counters")
	}
}
func TestDualScopedCooldownSkipsSlotsDoesNotReassign(t *testing.T) {
	b, s, job, now, ctx := dualFixture(t, 2, "")
	b.appleErr = &BackendError{Status: 429, Code: "UPSTREAM_RATE_LIMITED", RetryAfter: 2 * time.Hour}
	for i := 0; i < 12; i++ {
		driveDualSlot(s, job, now, ctx)
	}
	if job.state.Status != "completed" || b.appleCalls != 1 || job.dual.ChannelCreated[channelWeb] != 2 {
		t.Fatal(job.state, b.appleCalls)
	}
	if !job.dual.Paused[channelApple].After(now.Add(time.Hour)) {
		t.Fatal("shortened Retry-After")
	}
}
func TestDualDegradesToFiveWebAndRecovers(t *testing.T) {
	b, s, job, now, ctx := dualFixture(t, 28, "")
	b.accounts[0].HasAppleSession = false
	for i := 0; i < 23; i++ {
		driveDualSlot(s, job, now, ctx)
	}
	if job.state.Created != 5 || b.appleCalls != 0 {
		t.Fatal(job.state)
	}
	b.accounts[0].HasAppleSession = true
	// Rolling Web budget may defer a boundary slot, but no more than 19+4 attempts.
	oldNew := job.dual.ChannelCreated[channelApple]
	oldWeb := job.dual.ChannelCreated[channelWeb]
	for i := 0; i < 23; i++ {
		driveDualSlot(s, job, now, ctx)
	}
	if job.dual.ChannelCreated[channelApple]-oldNew != 19 || job.dual.ChannelCreated[channelWeb]-oldWeb > 4 || job.state.ScheduleMode != "dual" {
		t.Fatal(job.state, job.dual.ChannelCreated)
	}
}
func TestDualOfflineSkipsWithoutRemoteCalls(t *testing.T) {
	b, s, job, now, ctx := dualFixture(t, 5, "")
	b.accounts[0].HasCookies = false
	for i := 0; i < 23; i++ {
		driveDualSlot(s, job, now, ctx)
	}
	if len(job.dual.Attempts) != 0 || b.appleCalls != 0 || job.state.Status != "running" || job.state.ScheduleMode != "offline" {
		t.Fatal(job.state)
	}
	b.accounts[0].HasCookies = true
	driveDualSlot(s, job, now, ctx)
	if job.state.Created != 1 {
		t.Fatal("did not recover")
	}
}
func TestDualSessionRejectionPreservesOptIn(t *testing.T) {
	b, s, job, now, ctx := dualFixture(t, 10, "")
	b.appleErr = &BackendError{Status: 401, Code: "APPLE_ACCOUNT_LOGIN_REQUIRED"}
	for i := 0; i < 23; i++ {
		driveDualSlot(s, job, now, ctx)
	}
	if job.state.Created != 5 || job.state.ScheduleMode != "web_only" || b.appleCalls != 1 {
		t.Fatal(job.state, b.appleCalls)
	}
	b.accounts[0].HasAppleSession = false
	driveDualSlot(s, job, now, ctx)
	b.accounts[0].HasAppleSession = true
	b.appleErr = nil
	driveDualSlot(s, job, now, ctx)
	if job.state.ScheduleMode != "dual" || b.appleCalls != 2 {
		t.Fatal("login recovery", job.state)
	}
}
func TestDualSharedThrottleReplansWholeAccount(t *testing.T) {
	b, s, job, now, ctx := dualFixture(t, 5, "")
	b.listErr = &BackendError{Status: 429, Code: "UPSTREAM_RATE_LIMITED", RetryAfter: 2 * time.Hour}
	driveDualSlot(s, job, now, ctx)
	if !job.dual.Plan[0].Equal(now.Add(2*time.Hour)) || job.dual.Slot != 0 || b.appleCalls != 0 {
		t.Fatal(job.dual)
	}
}
func TestDualAmbiguousResultStopsWithoutFallback(t *testing.T) {
	b, s, job, now, ctx := dualFixture(t, 5, "")
	b.appleErr = &BackendError{Status: 502, Code: "ALIAS_CONFIRMATION_REQUIRED"}
	if !driveDualSlot(s, job, now, ctx) || job.state.Status != "failed" || job.state.Created != 0 || b.appleCalls != 1 {
		t.Fatal(job.state)
	}
}
func TestDualOverdueReschedulesWithoutBurst(t *testing.T) {
	b, s, job, now, ctx := dualFixture(t, 5, "")
	*now = now.Add(4 * time.Hour)
	delay, done := s.processDualSlot(ctx, job)
	if done || delay < dualMinimumInterval || b.appleCalls != 0 || !job.dual.Plan[22].Equal(now.Add(time.Hour)) {
		t.Fatal(job.state, delay)
	}
}
func TestDualRollingBudgetsAndBoundary(t *testing.T) {
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	r := &dualScheduleRecord{Paused: map[string]time.Time{}}
	for i := 0; i < 19; i++ {
		r.Attempts = append(r.Attempts, dualAttempt{At: now.Add(-50 * time.Minute).Add(time.Duration(i) * 2 * time.Minute), Channel: channelApple})
	}
	_, newUntil := r.budgetUntil(now, channelApple, false)
	_, webUntil := r.budgetUntil(now, channelWeb, false)
	if !newUntil.Equal(now.Add(10*time.Minute)) || webUntil.After(now) {
		t.Fatal(newUntil, webUntil)
	}
	_, after := r.budgetUntil(now.Add(10*time.Minute), channelApple, false)
	if after.After(now.Add(10 * time.Minute)) {
		t.Fatal("rolling boundary retained old attempt")
	}
	r.Attempts = nil
	for i := 0; i < 5; i++ {
		r.Attempts = append(r.Attempts, dualAttempt{At: now.Add(-40 * time.Minute).Add(time.Duration(i) * 5 * time.Minute), Channel: channelWeb})
	}
	_, dual := r.budgetUntil(now, channelWeb, false)
	_, only := r.budgetUntil(now, channelWeb, true)
	if !dual.Equal(now.Add(25*time.Minute)) || !only.Equal(now.Add(20*time.Minute)) {
		t.Fatal(dual, only)
	}
}
func TestDualPersistenceRetainsPlanLedgerAndCrashPause(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dual.json")
	b, s, job, now, ctx := dualFixture(t, 23, path)
	for i := 0; i < 7; i++ {
		driveDualSlot(s, job, now, ctx)
	}
	want := job.dual.Plan[job.dual.Slot]
	slot := job.dual.Slot
	attempts := len(job.dual.Attempts)
	s.Close()
	s2 := newWithBackend(b, Config{AdminPassword: "admin-pass-2026-strong", DualScheduleFile: path})
	defer s2.Close()
	s2.aliasBatches.dualNow = func() time.Time { return *now }
	if err := s2.restoreDualSchedules(false); err != nil {
		t.Fatal(err)
	}
	restored := s2.aliasBatches.jobs["acc_1"]
	if restored.dual.Slot != slot || !restored.dual.Plan[slot].Equal(want) || len(restored.dual.Attempts) != attempts || !batchActive(restored.state.Status) {
		t.Fatal("restart reset plan")
	}
	restored.dual.InFlight = true
	s2.aliasBatches.mu.Lock()
	s2.checkpointDualLocked(restored)
	s2.aliasBatches.mu.Unlock()
	s3 := newWithBackend(b, Config{AdminPassword: "admin-pass-2026-strong", DualScheduleFile: path})
	defer s3.Close()
	if err := s3.restoreDualSchedules(false); err != nil {
		t.Fatal(err)
	}
	if s3.aliasBatches.jobs["acc_1"].state.ErrorCode != "ALIAS_CONFIRMATION_REQUIRED" {
		t.Fatal("replayed uncertain call")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("file permissions", err)
	}
}
func TestDualStopPersistsAndReplacementKeepsBudgets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dual.json")
	b, s, job, now, ctx := dualFixture(t, 23, path)
	driveDualSlot(s, job, now, ctx)
	job.cancel()
	s.processDualSlot(ctx, job)
	if job.state.Status != "stopped" {
		t.Fatal(job.state)
	}
	s2 := newWithBackend(b, Config{AdminPassword: "admin-pass-2026-strong", DualScheduleFile: path})
	defer s2.Close()
	if err := s2.restoreDualSchedules(false); err != nil {
		t.Fatal(err)
	}
	if s2.aliasBatches.jobs["acc_1"].state.Status != "stopped" {
		t.Fatal("manual stop resumed")
	}
	replacement := &aliasBatchJob{state: aliasBatchStatus{ID: "replacement", AccountID: "acc_1", Channel: channelAuto, Status: "running", Count: 5}, cancel: func() {}}
	s2.aliasBatches.mu.Lock()
	s2.initializeDualLocked(replacement, aliasBatchRequest{AccountID: "acc_1", Channel: channelAuto, Count: 5})
	s2.aliasBatches.mu.Unlock()
	if len(replacement.dual.Attempts) != 1 {
		t.Fatal("replacement cleared hourly quota")
	}
}
func TestDualPersistenceFailurePreventsRemoteCreation(t *testing.T) {
	b, s, job, now, ctx := dualFixture(t, 5, "")
	s.aliasBatches.dualPath = filepath.Join(t.TempDir(), "missing", "dual.json")
	if !driveDualSlot(s, job, now, ctx) || b.appleCalls != 0 || job.state.ErrorCode != "SCHEDULE_SAVE_FAILED" {
		t.Fatal(job.state)
	}
}

func TestDualWebLoginRecoveryWithoutRestartingJob(t *testing.T) {
	b, s, job, now, ctx := dualFixture(t, 5, "")
	b.listErr = &BackendError{Status: 401, Code: "UPSTREAM_UNAUTHORIZED"}
	driveDualSlot(s, job, now, ctx)
	for i := 0; i < 5; i++ {
		driveDualSlot(s, job, now, ctx)
	}
	if len(job.dual.Attempts) != 1 || job.state.ScheduleMode != "offline" {
		t.Fatal(job.state)
	}
	b.listErr = nil
	b.accounts[0].LastValidated = "new-cookie-session"
	driveDualSlot(s, job, now, ctx)
	if job.state.Created != 1 || job.state.ScheduleMode != "dual" {
		t.Fatal("Web login failed to resume", job.state)
	}
}
func TestDualGlobalHourlyAndDailyLimits(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	r := &dualScheduleRecord{Paused: map[string]time.Time{}}
	for i := 0; i < 23; i++ {
		r.Attempts = append(r.Attempts, dualAttempt{At: now.Add(-59 * time.Minute).Add(time.Duration(i) * 2 * time.Minute), Channel: channelApple})
	}
	global, _ := r.budgetUntil(now, channelWeb, false)
	if !global.Equal(now.Add(time.Minute)) {
		t.Fatal(global)
	}
	r.Attempts = nil
	for i := 0; i < dualDailyLimit; i++ {
		r.Attempts = append(r.Attempts, dualAttempt{At: now.Add(-23 * time.Hour).Add(time.Duration(i) * 2 * time.Minute), Channel: channelApple})
	}
	global, _ = r.budgetUntil(now, channelWeb, false)
	if !global.Equal(now.Add(time.Hour)) {
		t.Fatal(global)
	}
}
func TestDualCorruptPersistenceFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dual.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"records":{"acc_1":{"slot":22}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	b := &channelFake{}
	s := newWithBackend(b, Config{AdminPassword: "admin-pass-2026-strong", DualScheduleFile: path})
	defer s.Close()
	if err := s.restoreDualSchedules(true); err == nil {
		t.Fatal("corrupt schedule accepted")
	}
	if b.appleCalls != 0 {
		t.Fatal("network side effect")
	}
}

func TestDualHTTPStartNormalizesPolicyAndStopIsDurable(t *testing.T) {
	b := &channelFake{}
	b.accounts = []account.Summary{{ID: "acc_1", HasCookies: true}}
	path := filepath.Join(t.TempDir(), "dual.json")
	s := newWithBackend(b, Config{AdminPassword: "admin-pass-2026-strong", SessionTTL: time.Hour, DualScheduleFile: path})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	defer s.Close()
	cookie, csrf := login(t, ts, "admin-pass-2026-strong")
	request := func(path, body string) (int, string) {
		req := authedReq(t, ts, "POST", path, body)
		req.AddCookie(&http.Cookie{Name: "hme_session", Value: cookie})
		req.Header.Set("X-CSRF-Token", csrf)
		status, response, _ := do(t, req)
		return status, response
	}
	status, response := request("/api/alias-batch", `{"account_id":"acc_1","channel":"auto","count":2,"interval_seconds":1,"cooldown_minutes":1}`)
	if status != 202 {
		t.Fatal(status, response)
	}
	var envelope struct {
		Data aliasBatchStatus `json:"data"`
	}
	if err := json.Unmarshal([]byte(response), &envelope); err != nil {
		t.Fatal(err)
	}
	state := envelope.Data
	if state.Policy != "upstream_19_4" || state.IntervalSeconds != 120 || state.CooldownMinutes != 60 || state.NextRunAt == nil || !state.NextRunAt.After(time.Now().Add(119*time.Second)) {
		t.Fatal(state)
	}
	if b.count() != 0 {
		t.Fatal("immediate generation")
	}
	status, response = request("/api/alias-batch/stop", fmt.Sprintf(`{"account_id":"acc_1","job_id":%q}`, state.ID))
	if status != 200 {
		t.Fatal(status, response)
	}
	waitBatch(t, s, func(state aliasBatchStatus) bool { return state.Status == "stopped" })
	s.Close()
	restored := newWithBackend(b, Config{AdminPassword: "admin-pass-2026-strong", DualScheduleFile: path})
	defer restored.Close()
	if err := restored.restoreDualSchedules(true); err != nil {
		t.Fatal(err)
	}
	if restored.aliasBatches.jobs["acc_1"].state.Status != "stopped" {
		t.Fatal("stop not durable")
	}
}
