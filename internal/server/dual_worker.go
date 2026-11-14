package server

import (
	"context"
	"fmt"
	"icloud-hme/internal/account"
	"time"
)

type dualAvailability struct {
	exists, web, apple         bool
	webRevision, appleRevision string
}

func (s *Server) dualAvailability(id string) dualAvailability {
	var a account.Summary
	found := false
	for _, v := range s.be.ListAccounts() {
		if v.ID == id {
			a = v
			found = true
			break
		}
	}
	out := dualAvailability{exists: found, web: a.HasCookies, apple: a.HasAppleSession, webRevision: a.LastValidated, appleRevision: fmt.Sprint(a.HasAppleSession)}
	if b, ok := s.be.(*managerBackend); ok {
		if full, ok := b.mgr.GetAccount(id); ok && full.AppleSession != nil {
			out.appleRevision = full.AppleSession.AuthenticatedAt.UTC().Format(time.RFC3339Nano)
		}
	}
	return out
}
func (s *Server) dualViewLocked(job *aliasBatchJob, availability dualAvailability, now time.Time) {
	r := job.dual
	if r.WebRejected && r.WebRevision == availability.webRevision {
		availability.web = false
	}
	if r.AppleRejected && r.AppleRevision == availability.appleRevision {
		availability.apple = false
	}
	mode := "dual"
	if !availability.apple {
		mode = "web_only"
	}
	if !availability.web {
		mode = "offline"
	}
	job.state.ScheduleMode = mode
	job.state.ScheduleSlot = r.Slot + 1
	channels := make([]creationChannelStatus, 0, 2)
	for _, name := range []string{channelApple, channelWeb} {
		_, until := r.budgetUntil(now, name, !availability.apple)
		until = laterDualTime(until, r.Paused["all"])
		ch := creationChannelStatus{Channel: name, Created: r.ChannelCreated[name], Disabled: !availability.web || name == channelApple && !availability.apple}
		if until.After(now) {
			t := until
			ch.NextRunAt = &t
			ch.Message = "通道冷却或滚动小时额度等待"
		}
		channels = append(channels, ch)
	}
	job.state.Channels = channels
	next := r.Plan[r.Slot]
	job.state.NextRunAt = &next
}

// A single step is clock-driven for deterministic restart/budget tests.
// Records are committed before generation, while all creation entry points
// still share the existing per-account lock and authoritative quota check.
func (s *Server) processDualSlot(ctx context.Context, job *aliasBatchJob) (time.Duration, bool) {
	m := s.aliasBatches
	availability := s.dualAvailability(job.state.AccountID)
	m.mu.Lock()
	r := job.dual
	now := m.dualNow().UTC()
	if m.closing {
		m.mu.Unlock()
		return 0, true
	}
	if ctx.Err() != nil || job.state.Status == "stopping" {
		s.finishDualLocked(job, "stopped", "", "已停止自动注册")
		m.mu.Unlock()
		return 0, true
	}
	if !batchActive(job.state.Status) {
		m.mu.Unlock()
		return 0, true
	}
	if !availability.exists {
		s.finishDualLocked(job, "failed", "ACCOUNT_NOT_FOUND", "账号不存在")
		m.mu.Unlock()
		return 0, true
	}
	if r.WebRejected && r.WebRevision != availability.webRevision {
		r.WebRejected = false
	}
	if r.AppleRejected && r.AppleRevision != availability.appleRevision {
		r.AppleRejected = false
	}
	availability.web = availability.web && !r.WebRejected
	availability.apple = availability.apple && !r.AppleRejected
	s.dualViewLocked(job, availability, now)
	due := r.Plan[r.Slot]
	if now.Before(due) {
		m.mu.Unlock()
		return due.Sub(now), false
	}
	if now.Sub(due) > dualOverdueGrace {
		r.replan(now, false)
		job.state.Message = "错过的计划不补发，已重新安排下一小时"
		s.dualViewLocked(job, availability, now)
		ok := s.checkpointDualLocked(job)
		delay := job.state.NextRunAt
		m.mu.Unlock()
		if !ok {
			return 0, true
		}
		return delay.Sub(now), false
	}
	slot := r.Slot
	channel := dualSlotChannel(slot)
	skip := !availability.web || !availability.apple && !dualWebOnlySlot(slot)
	if !availability.apple {
		channel = channelWeb
	}
	global, scoped := r.budgetUntil(now, channel, !availability.apple)
	if !skip && global.After(now) {
		r.replan(global, true)
		job.state.Status = "cooling_down"
		job.state.Message = "共享创建额度或最小间隔等待"
		s.dualViewLocked(job, availability, now)
		ok := s.checkpointDualLocked(job)
		m.mu.Unlock()
		return global.Sub(now), !ok
	}
	r.advance(now)
	if skip || scoped.After(now) {
		job.state.Status = "running"
		switch {
		case !availability.web:
			job.state.Message = "旧通道会话不可用，本时隙跳过；重新登录后自动恢复"
		case !availability.apple && skip:
			job.state.Message = "仅旧通道可用，按每小时 5 个分散时隙运行"
		default:
			job.state.Message = "当前通道冷却或额度用尽，跳过该时隙，不占用另一通道计划"
		}
		s.dualViewLocked(job, availability, now)
		ok := s.checkpointDualLocked(job)
		next := r.Plan[r.Slot]
		m.mu.Unlock()
		return next.Sub(now), !ok
	}
	// Reserve the rolling budget and crash checkpoint before the remote mutation.
	kept := make([]dualAttempt, 0, len(r.Attempts)+1)
	for _, a := range r.Attempts {
		if a.At.After(now.Add(-24 * time.Hour)) {
			kept = append(kept, a)
		}
	}
	r.Attempts = append(kept, dualAttempt{At: now, Channel: channel})
	r.LastAttempt = now
	r.InFlight = true
	job.state.Status = "running"
	job.state.ErrorCode = ""
	job.state.Message = "按原项目 19+4 小时计划创建"
	label := fmt.Sprintf("%s-%s-%03d", r.Request.LabelPrefix, job.state.ID, job.state.Created+1)
	s.dualViewLocked(job, availability, now)
	if !s.checkpointDualLocked(job) {
		m.mu.Unlock()
		return 0, true
	}
	m.mu.Unlock()
	result, total, err := s.createBatchAlias(ctx, job.state.AccountID, label, false, channel)
	m.mu.Lock()
	defer m.mu.Unlock()
	observed := m.dualNow().UTC()
	// Shutdown preserves the already durable in-flight marker; recovery pauses it.
	if m.closing {
		return 0, true
	}
	r.InFlight = false
	if err == nil {
		job.state.Created++
		job.state.AliasTotal = total
		job.state.LastEmail = result.Email
		r.ChannelCreated[channel]++
		job.state.ErrorCode = ""
		job.state.Message = ""
	} else {
		e := asBackendError(err)
		job.state.ErrorCode = e.Code
		job.state.Message = e.Message
		switch e.Code {
		case "UPSTREAM_RATE_LIMITED", "RATE_LIMITED":
			until := observed.Add(max(time.Hour, e.RetryAfter))
			scope := e.CreationChannel
			if scope == "" {
				scope = "all"
				job.state.Status = "cooling_down"
				r.replan(until, true)
			}
			r.Paused[scope] = laterDualTime(r.Paused[scope], until)
			job.state.CooldownCount++
		case "APPLE_ACCOUNT_LOGIN_REQUIRED":
			r.AppleRejected = true
			r.AppleRevision = availability.appleRevision
			job.state.Message = "新通道会话失效，降级为旧通道每小时 5 个；新登录恢复后自动切回 19+4"
		case "UPSTREAM_UNAUTHORIZED", "COOKIES_REQUIRED":
			r.WebRejected = true
			r.WebRevision = availability.webRevision
			job.state.Message = "旧通道会话失效，保留计划并跳过时隙；更新登录后自动恢复"
		case "UPSTREAM_FAILURE":
			if e.CreationChannel != "" {
				s.finishDualLocked(job, "failed", "ALIAS_CONFIRMATION_REQUIRED", "创建结果待确认，任务已暂停，请核对别名列表")
				return 0, true
			}
			// Shared read-only failures consume this deadline, not a fresh generation.
		case "ALIAS_LIMIT_REACHED":
			job.state.AliasTotal = total
			s.finishDualLocked(job, "limit_reached", e.Code, e.Message)
			return 0, true
		default:
			if ctx.Err() == nil {
				s.finishDualLocked(job, "failed", e.Code, e.Message)
				return 0, true
			}
		}
	}
	s.dualViewLocked(job, availability, observed)
	if ctx.Err() != nil || job.state.Status == "stopping" {
		s.finishDualLocked(job, "stopped", "", "已停止自动注册")
		return 0, true
	}
	if job.state.Created >= r.Request.Count {
		s.finishDualLocked(job, "completed", "", "本次自动注册已完成")
		return 0, true
	}
	// Slow upstream calls must never compress following requests into a burst.
	if earliest := observed.Add(dualMinimumInterval); r.Plan[r.Slot].Before(earliest) {
		delta := earliest.Sub(r.Plan[r.Slot])
		for i := r.Slot; i < len(r.Plan); i++ {
			r.Plan[i] = r.Plan[i].Add(delta)
		}
	}
	s.dualViewLocked(job, availability, observed)
	ok := s.checkpointDualLocked(job)
	return r.Plan[r.Slot].Sub(observed), !ok
}
