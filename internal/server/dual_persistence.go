package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// All durable records and job state are protected by aliasBatches.mu.
// Commit intent before a remote call. An interrupted in-flight call is paused
// on restart for explicit confirmation, never silently replayed.
func (s *Server) saveDualLocked() error {
	m := s.aliasBatches
	if m.dualPath == "" {
		return nil
	}
	raw, err := json.Marshal(struct {
		Version int                            `json:"version"`
		Records map[string]*dualScheduleRecord `json:"records"`
	}{1, m.dualRecords})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(m.dualPath), ".dual-schedule-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), m.dualPath); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(m.dualPath))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (s *Server) checkpointDualLocked(job *aliasBatchJob) bool {
	job.dual.State = job.state
	if err := s.saveDualLocked(); err != nil {
		now := s.aliasBatches.dualNow().UTC()
		job.state.Status = "failed"
		job.state.ErrorCode = "SCHEDULE_SAVE_FAILED"
		job.state.Message = "计划保存失败，已停止创建；请检查数据目录后核对任务状态"
		job.state.NextRunAt = nil
		job.state.FinishedAt = &now
		job.dual.State = job.state
		return false
	}
	return true
}
func (s *Server) finishDualLocked(job *aliasBatchJob, status, code, message string) {
	now := s.aliasBatches.dualNow().UTC()
	job.state.Status = status
	job.state.ErrorCode = code
	job.state.Message = message
	job.state.NextRunAt = nil
	job.state.FinishedAt = &now
	s.checkpointDualLocked(job)
}
func (s *Server) initializeDualLocked(job *aliasBatchJob, req aliasBatchRequest) bool {
	now := s.aliasBatches.dualNow().UTC()
	r := &dualScheduleRecord{Request: req, Paused: map[string]time.Time{}, ChannelCreated: map[string]int{}}
	if old := s.aliasBatches.dualRecords[req.AccountID]; old != nil {
		r.Attempts = append([]dualAttempt(nil), old.Attempts...)
		r.LastAttempt = old.LastAttempt
		for key, t := range old.Paused {
			r.Paused[key] = t
		}
	}
	r.replan(now, false)
	job.dual = r
	s.aliasBatches.dualRecords[req.AccountID] = r
	job.state.Continuous = req.Continuous
	job.state.Count = req.Count
	job.state.Policy = "upstream_19_4"
	job.state.IntervalSeconds = 120
	job.state.CooldownMinutes = 60
	job.state.BatchSize = 23
	next := r.Plan[0]
	job.state.NextRunAt = &next
	return s.checkpointDualLocked(job)
}
func validateDualRecord(id string, r *dualScheduleRecord) error {
	if r == nil || r.Request.AccountID != id || r.Request.Channel != channelAuto || r.State.AccountID != id || r.State.Channel != channelAuto || r.State.ID == "" || r.State.Created < 0 || len(r.Plan) != dualSlots || r.Slot < 0 || r.Slot >= dualSlots {
		return fmt.Errorf("invalid dual schedule record")
	}
	if r.Request.Continuous != r.State.Continuous || r.State.Count != r.Request.Count {
		return fmt.Errorf("inconsistent dual schedule mode")
	}
	if r.Request.Continuous {
		if r.Request.Count != 0 {
			return fmt.Errorf("continuous schedule must not have a target count")
		}
	} else if r.Request.Count < 1 || r.Request.Count > MaxAliases || r.State.Created > r.Request.Count {
		return fmt.Errorf("invalid finite schedule target")
	}
	for i, t := range r.Plan {
		if t.IsZero() || i > 0 && t.Sub(r.Plan[i-1]) < dualMinimumInterval {
			return fmt.Errorf("invalid dual schedule deadlines")
		}
	}
	for i, a := range r.Attempts {
		if a.At.IsZero() || (a.Channel != channelApple && a.Channel != channelWeb) || i > 0 && a.At.Before(r.Attempts[i-1].At) {
			return fmt.Errorf("invalid dual schedule budget")
		}
	}
	return nil
}
func (s *Server) restoreDualSchedules(startWorkers bool) error {
	m := s.aliasBatches
	if m.dualPath == "" {
		return nil
	}
	raw, err := os.ReadFile(m.dualPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var file struct {
		Version int                            `json:"version"`
		Records map[string]*dualScheduleRecord `json:"records"`
	}
	if err = json.Unmarshal(raw, &file); err != nil {
		return err
	}
	if file.Version != 1 {
		return fmt.Errorf("unsupported dual schedule version")
	}
	// Validate the entire file before scheduling any network work.
	for id, r := range file.Records {
		if err = validateDualRecord(id, r); err != nil {
			return err
		}
	}
	m.mu.Lock()
	if file.Records != nil {
		m.dualRecords = file.Records
	}
	var resume []*aliasBatchJob
	for id, r := range m.dualRecords {
		if r.Paused == nil {
			r.Paused = map[string]time.Time{}
		}
		if r.ChannelCreated == nil {
			r.ChannelCreated = map[string]int{}
		}
		ctx, cancel := context.WithCancel(context.Background())
		job := &aliasBatchJob{state: r.State, dual: r, cancel: cancel}
		m.jobs[id] = job
		if r.InFlight {
			// A crash between generation and response/local save must not duplicate it.
			r.InFlight = false
			s.finishDualLocked(job, "failed", "ALIAS_CONFIRMATION_REQUIRED", "重启前的创建结果待确认，请先核对别名列表再启动")
		} else if batchActive(job.state.Status) {
			if job.state.Status == "stopping" {
				s.finishDualLocked(job, "stopped", "", "已停止自动注册")
			} else if startWorkers {
				job.done = make(chan struct{})
				job.workerCtx = ctx
				resume = append(resume, job)
			}
		}
	}
	// Persist crash/stop decisions before starting any workers.
	err = s.saveDualLocked()
	m.mu.Unlock()
	if err != nil {
		return err
	}
	for _, job := range resume {
		go s.runDualBatch(job.workerCtx, job)
	}
	return nil
}

// Only wait here, never call the legacy wait helper (it finishes on cancellation
// and would erase the opt-in on shutdown).
func (s *Server) runDualBatch(ctx context.Context, job *aliasBatchJob) {
	if job.done != nil {
		defer close(job.done)
	}
	defer job.cancel()
	for {
		delay, done := s.processDualSlot(ctx, job)
		if done {
			return
		}
		timer := time.NewTimer(max(delay, time.Millisecond))
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}
