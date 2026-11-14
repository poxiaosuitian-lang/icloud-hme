package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"icloud-hme/internal/hme"
)

func continuousFixture(t *testing.T, path string) (*channelFake, *Server, *aliasBatchJob, *time.Time, context.Context) {
	t.Helper()
	b, s, job, now, ctx := dualFixture(t, 1, path)
	job.dual.Request.Continuous = true
	job.dual.Request.Count = 0
	job.state.Continuous = true
	job.state.Count = 0
	s.aliasBatches.mu.Lock()
	ok := s.checkpointDualLocked(job)
	s.aliasBatches.mu.Unlock()
	if !ok {
		t.Fatal("continuous checkpoint")
	}
	return b, s, job, now, ctx
}
func TestContinuousDualCyclesUntilManualStop(t *testing.T) {
	b, s, job, now, ctx := continuousFixture(t, "")
	for i := 0; i < 23; i++ {
		if driveDualSlot(s, job, now, ctx) {
			t.Fatal("continuous job ended", i)
		}
	}
	if job.state.Created != 23 || job.dual.ChannelCreated[channelApple] != 19 || job.dual.ChannelCreated[channelWeb] != 4 {
		t.Fatal("first hour is not 19+4")
	}
	for i := 0; i < 46; i++ {
		if driveDualSlot(s, job, now, ctx) {
			t.Fatal("continuous job ended in a later cycle")
		}
	}
	if job.state.Created <= 23 || job.dual.ChannelCreated[channelWeb] <= 4 || !batchActive(job.state.Status) {
		t.Fatal("next cycle did not continue")
	}
	job.cancel()
	if _, done := s.processDualSlot(ctx, job); !done || job.state.Status != "stopped" {
		t.Fatal("manual stop not honored")
	}
	calls := b.appleCalls
	for i := 0; i < 3; i++ {
		s.processDualSlot(ctx, job)
	}
	if b.appleCalls != calls {
		t.Fatal("creation after stop")
	}
}
func TestContinuousDualCapacityWaitAndResume(t *testing.T) {
	b, s, job, now, ctx := continuousFixture(t, "")
	b.items = make([]hme.Alias, MaxAliases)
	if driveDualSlot(s, job, now, ctx) {
		t.Fatal("capacity ended opt-in")
	}
	if job.state.Status != "cooling_down" || job.state.ErrorCode != "ALIAS_LIMIT_REACHED" || b.appleCalls != 0 || job.state.Created != 0 {
		t.Fatal("capacity handling", job.state)
	}
	if job.state.NextRunAt == nil || job.state.NextRunAt.Before(now.Add(time.Hour)) {
		t.Fatal("capacity tight retry")
	}
	*now = now.Add(30 * time.Minute)
	if _, done := s.processDualSlot(ctx, job); done || b.appleCalls != 0 {
		t.Fatal("creation during capacity wait")
	}
	b.items = b.items[:MaxAliases-1]
	if driveDualSlot(s, job, now, ctx) || job.state.Created != 1 || b.appleCalls != 1 {
		t.Fatal("capacity release did not resume")
	}
	if driveDualSlot(s, job, now, ctx) || job.state.Status != "cooling_down" {
		t.Fatal("full capacity did not wait again")
	}
}
func TestContinuousDualRestartRetainsLargeCountersAndStop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dual.json")
	b, s, job, now, ctx := continuousFixture(t, path)
	driveDualSlot(s, job, now, ctx)
	job.state.Created = 1001
	job.dual.ChannelCreated[channelApple] = 1001
	s.aliasBatches.mu.Lock()
	s.checkpointDualLocked(job)
	s.aliasBatches.mu.Unlock()
	s.Close()
	s2 := newWithBackend(b, Config{AdminPassword: "fixture-password", DualScheduleFile: path})
	defer s2.Close()
	s2.aliasBatches.dualNow = func() time.Time { return *now }
	if err := s2.restoreDualSchedules(false); err != nil {
		t.Fatal(err)
	}
	restored := s2.aliasBatches.jobs["acc_1"]
	if !restored.state.Continuous || restored.state.Created != 1001 || restored.state.Count != 0 || !batchActive(restored.state.Status) {
		t.Fatal("continuous restart lost opt-in")
	}
	if restored.dual.LastAttempt != job.dual.LastAttempt || len(restored.dual.Attempts) != len(job.dual.Attempts) {
		t.Fatal("restart reset budgets")
	}
	s2.aliasBatches.mu.Lock()
	s2.finishDualLocked(restored, "stopped", "", "manual stop")
	s2.aliasBatches.mu.Unlock()
	s3 := newWithBackend(b, Config{AdminPassword: "fixture-password", DualScheduleFile: path})
	defer s3.Close()
	if err := s3.restoreDualSchedules(false); err != nil {
		t.Fatal(err)
	}
	if s3.aliasBatches.jobs["acc_1"].state.Status != "stopped" {
		t.Fatal("stopped continuous job resumed")
	}
}
func TestContinuousDualAmbiguityStillRequiresConfirmation(t *testing.T) {
	b, s, job, now, ctx := continuousFixture(t, "")
	b.appleErr = &BackendError{Status: 502, Code: "ALIAS_CONFIRMATION_REQUIRED"}
	if !driveDualSlot(s, job, now, ctx) || job.state.ErrorCode != "ALIAS_CONFIRMATION_REQUIRED" {
		t.Fatal("ambiguous mutation was retried")
	}
}
func TestContinuousNotAcceptedForSingleChannel(t *testing.T) {
	_, _, request := batchTestServer(t, 0)
	for _, channel := range []string{"icloud_web", "apple_account"} {
		status, _ := request("POST", "/api/alias-batch", `{"account_id":"acc_1","channel":"`+channel+`","continuous":true,"count":1,"interval_seconds":1}`)
		if status != 400 {
			t.Fatal("continuous single channel accepted", status)
		}
	}
}
func TestContinuousRecordRejectsMixedFiniteTarget(t *testing.T) {
	_, _, job, _, _ := continuousFixture(t, "")
	if err := validateDualRecord("acc_1", job.dual); err != nil {
		t.Fatal(err)
	}
	job.dual.Request.Count = 5
	if validateDualRecord("acc_1", job.dual) == nil {
		t.Fatal("mixed target accepted")
	}
}
