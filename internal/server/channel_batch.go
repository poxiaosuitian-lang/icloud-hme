package server

import (
	"context"
	"fmt"
	"time"
)

type creationChannelStatus struct {
	Channel   string     `json:"channel"`
	Created   int        `json:"created"`
	NextRunAt *time.Time `json:"next_run_at,omitempty"`
	ErrorCode string     `json:"error_code,omitempty"`
	Message   string     `json:"message,omitempty"`
	Disabled  bool       `json:"disabled"`
	failures  int
}

// Publish immutable value snapshots; polling never races worker mutations.
func (s *Server) publishChannels(job *aliasBatchJob, channels []creationChannelStatus) {
	s.aliasBatches.mu.Lock()
	defer s.aliasBatches.mu.Unlock()
	job.state.Channels = append([]creationChannelStatus(nil), channels...)
}

// Each channel gets its own conservative five-item local batch and cooldown.
// Apple's Retry-After always wins. Shared directory errors stop both channels.
func (s *Server) runChannelBatch(ctx context.Context, job *aliasBatchJob, req aliasBatchRequest) {
	defer job.cancel()
	names, err := s.creationChannels(req.AccountID, req.Channel)
	if err != nil {
		e := asBackendError(err)
		s.finishAliasBatch(job, "failed", e.Code, e.Message)
		return
	}
	channels := make([]creationChannelStatus, len(names))
	for i, n := range names {
		channels[i].Channel = n
	}
	interval := time.Duration(req.IntervalSeconds * float64(time.Second))
	cooldown := time.Duration(req.CooldownMinutes) * s.aliasBatches.minuteDuration
	nextIndex := 0
	for created := 0; created < req.Count; {
		if ctx.Err() != nil {
			s.finishAliasBatch(job, "stopped", "", "已停止自动注册")
			return
		}
		now := time.Now()
		chosen := -1
		var earliest time.Time
		for offset := 0; offset < len(channels); offset++ {
			i := (nextIndex + offset) % len(channels)
			ch := channels[i]
			if ch.Disabled {
				continue
			}
			if ch.NextRunAt == nil || !ch.NextRunAt.After(now) {
				chosen = i
				break
			}
			if earliest.IsZero() || ch.NextRunAt.Before(earliest) {
				earliest = *ch.NextRunAt
			}
		}
		s.publishChannels(job, channels)
		if chosen < 0 {
			if earliest.IsZero() {
				s.finishAliasBatch(job, "failed", "CHANNELS_UNAVAILABLE", "所有创建通道均需重新登录或处理账号状态")
				return
			}
			if !s.waitAliasBatch(ctx, job, time.Until(earliest), true, "", "所有可用通道正在冷却") {
				return
			}
			continue
		}
		ch := &channels[chosen]
		nextIndex = (chosen + 1) % len(channels)
		label := fmt.Sprintf("%s-%s-%03d", req.LabelPrefix, job.state.ID, created+1)
		// List/quota checks are shared and must succeed before either channel mutates.
		result, total, err := s.createBatchAlias(ctx, req.AccountID, label, false, ch.Channel)
		if err != nil {
			if ctx.Err() != nil {
				s.finishAliasBatch(job, "stopped", "", "已停止自动注册")
				return
			}
			e := asBackendError(err)
			ch.ErrorCode = e.Code
			ch.Message = e.Message
			switch e.Code {
			case "UPSTREAM_RATE_LIMITED", "RATE_LIMITED":
				// The operation may be a shared directory read; conservatively pause both.
				ch.failures++
				next := time.Now().Add(cooldownDelay(cooldown, ch.failures, e.RetryAfter))
				ch.NextRunAt = &next
				// Only creation errors have a channel scope; shared reads do not.
				if e.CreationChannel == "" {
					for i := range channels {
						if channels[i].NextRunAt == nil || channels[i].NextRunAt.Before(next) {
							channels[i].NextRunAt = &next
						}
					}
				}
			case "APPLE_ACCOUNT_LOGIN_REQUIRED", "APPLE_ACCOUNT_ACTION_REQUIRED":
				ch.Disabled = true
			default:
				s.publishChannels(job, channels)
				status := "failed"
				if e.Code == "ALIAS_LIMIT_REACHED" {
					status = "limit_reached"
					s.aliasBatches.mu.Lock()
					job.state.AliasTotal = total
					s.aliasBatches.mu.Unlock()
				}
				s.finishAliasBatch(job, status, e.Code, e.Message)
				return
			}
		} else {
			created++
			ch.Created++
			ch.failures = 0
			ch.ErrorCode = ""
			ch.Message = ""
			ch.NextRunAt = nil
			if ch.Created%aliasBatchSize == 0 {
				next := time.Now().Add(max(interval, cooldown))
				ch.NextRunAt = &next
			}
			s.aliasBatches.mu.Lock()
			job.state.Created = created
			job.state.AliasTotal = total
			job.state.LastEmail = result.Email
			s.aliasBatches.mu.Unlock()
		}
		s.publishChannels(job, channels)
		if created == req.Count {
			s.finishAliasBatch(job, "completed", "", "本次自动注册已完成")
			return
		}
		if !s.waitAliasBatch(ctx, job, interval, false, "", "") {
			return
		}
	}
}
