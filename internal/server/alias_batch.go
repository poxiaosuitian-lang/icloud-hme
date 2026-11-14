package server

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"icloud-hme/internal/hme"
)

// MaxAliases counts both active and inactive aliases for each account.
const MaxAliases = 750

const aliasBatchSize = 5
const defaultCooldownMinutes = 60

type aliasBatchRequest struct {
	Continuous      bool    `json:"continuous"`
	Channel         string  `json:"channel"`
	AccountID       string  `json:"account_id"`
	Count           int     `json:"count"`
	IntervalSeconds float64 `json:"interval_seconds"`
	CooldownMinutes int     `json:"cooldown_minutes"`
	LabelPrefix     string  `json:"label_prefix"`
}

type aliasBatchStatus struct {
	Continuous      bool                    `json:"continuous"`
	Policy          string                  `json:"policy,omitempty"`
	ScheduleMode    string                  `json:"schedule_mode,omitempty"`
	ScheduleSlot    int                     `json:"schedule_slot,omitempty"`
	Channel         string                  `json:"channel"`
	Channels        []creationChannelStatus `json:"channels,omitempty"`
	ID              string                  `json:"id"`
	AccountID       string                  `json:"account_id"`
	Status          string                  `json:"status"`
	Count           int                     `json:"count"`
	Created         int                     `json:"created"`
	IntervalSeconds float64                 `json:"interval_seconds"`
	CooldownMinutes int                     `json:"cooldown_minutes"`
	BatchSize       int                     `json:"batch_size"`
	LabelPrefix     string                  `json:"label_prefix"`
	AliasTotal      int                     `json:"alias_total"`
	MaxAliases      int                     `json:"max_aliases"`
	LastEmail       string                  `json:"last_email,omitempty"`
	ErrorCode       string                  `json:"error_code,omitempty"`
	Message         string                  `json:"message,omitempty"`
	StartedAt       time.Time               `json:"started_at"`
	FinishedAt      *time.Time              `json:"finished_at,omitempty"`
	NextRunAt       *time.Time              `json:"next_run_at,omitempty"`
	CooldownCount   int                     `json:"cooldown_count"`
}

type aliasBatchJob struct {
	dual      *dualScheduleRecord
	done      chan struct{}
	workerCtx context.Context
	state     aliasBatchStatus
	cancel    context.CancelFunc
}

type aliasBatchManager struct {
	dualRecords    map[string]*dualScheduleRecord
	dualPath       string
	dualNow        func() time.Time
	closing        bool
	mu             sync.Mutex
	jobs           map[string]*aliasBatchJob
	creationLocks  sync.Map
	minuteDuration time.Duration // Real minutes in production; shortened by worker tests.
}

func newAliasBatchManager() *aliasBatchManager {
	return &aliasBatchManager{jobs: make(map[string]*aliasBatchJob), minuteDuration: time.Minute, dualRecords: make(map[string]*dualScheduleRecord), dualNow: time.Now}
}

func (m *aliasBatchManager) creationLock(accountID string) *sync.Mutex {
	lock, _ := m.creationLocks.LoadOrStore(accountID, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func batchActive(status string) bool {
	return status == "running" || status == "cooling_down" || status == "stopping"
}

// All creation entry points share this lock and authoritative quota check.
// A stop request cancels waiting/queued work, not an upstream request already sent.
func (s *Server) createAliasWithinLimit(ctx context.Context, accountID, label string) (*hme.CreateResult, int, error) {
	return s.createAliasWithinCustomLimit(ctx, accountID, label, MaxAliases)
}

func (s *Server) createAliasWithinCustomLimit(ctx context.Context, accountID, label string, limit int) (*hme.CreateResult, int, error) {
	lock := s.aliasBatches.creationLock(accountID)
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	aliases, err := s.be.ListAliases(accountID)
	if err != nil {
		return nil, 0, err
	}
	total := len(aliases)
	if total >= limit {
		return nil, total, &BackendError{Status: http.StatusConflict, Code: "ALIAS_LIMIT_REACHED", Message: fmt.Sprintf("该账号别名总数已达到 %d 个上限（含已停用别名）", limit)}
	}
	if err := ctx.Err(); err != nil {
		return nil, total, err
	}
	result, err := s.createOnChannel(ctx, accountID, label, channelWeb)
	if err != nil {
		return nil, total, err
	}
	if result == nil || result.Email == "" {
		return nil, total, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "创建邮箱未返回有效结果，请刷新别名列表后重试"}
	}
	return result, total + 1, nil
}

func (s *Server) hasBatchAccount(accountID string) bool {
	for _, acc := range s.be.ListAccounts() {
		if acc.ID == accountID {
			return true
		}
	}
	return false
}

func (s *Server) getAliasBatchHandler(c *gin.Context) {
	accountID := strings.TrimSpace(c.Query("account_id"))
	if accountID == "" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "account_id 必填")
		return
	}
	if !s.hasBatchAccount(accountID) {
		failCode(c, http.StatusNotFound, "ACCOUNT_NOT_FOUND", "账号不存在")
		return
	}
	s.aliasBatches.mu.Lock()
	job := s.aliasBatches.jobs[accountID]
	var state *aliasBatchStatus
	if job != nil {
		copy := job.state
		state = &copy
	}
	s.aliasBatches.mu.Unlock()
	ok(c, state)
}

func (s *Server) startAliasBatchHandler(c *gin.Context) {
	req := aliasBatchRequest{CooldownMinutes: defaultCooldownMinutes}
	if err := c.ShouldBindJSON(&req); err != nil {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "参数错误")
		return
	}
	var validChannel bool
	req.Channel, validChannel = normalizeCreationChannel(req.Channel)
	if !validChannel {
		failCode(c, 400, "VALIDATION_ERROR", "channel 需为 icloud_web、apple_account 或 auto")
		return
	}
	if req.Channel == channelAuto {
		// All newly started dual-channel jobs run until explicitly stopped.
		// Old persisted finite jobs retain their original contract on restart.
		req.Continuous = true
		req.Count = 0
		req.IntervalSeconds = 120
		req.CooldownMinutes = 60
	}
	req.AccountID = strings.TrimSpace(req.AccountID)
	req.LabelPrefix = strings.TrimSpace(req.LabelPrefix)
	if req.Continuous && req.Channel != channelAuto {
		failCode(c, 400, "VALIDATION_ERROR", "持续运行仅支持新旧双通道")
		return
	}
	if req.AccountID == "" || (!req.Continuous && (req.Count < 1 || req.Count > MaxAliases)) {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "account_id 必填，本次注册数量需为 1-750 的整数")
		return
	}
	if math.IsNaN(req.IntervalSeconds) || math.IsInf(req.IntervalSeconds, 0) || req.IntervalSeconds < 1 || req.IntervalSeconds > 86400 {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "每个间隔需为 1-86400 秒")
		return
	}
	if len([]rune(req.LabelPrefix)) > 170 {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "标签前缀最长 170 字符")
		return
	}
	if req.CooldownMinutes < 1 || req.CooldownMinutes > 1440 {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "冷却时长需为 1-1440 分钟的整数")
		return
	}
	if !s.hasBatchAccount(req.AccountID) {
		failCode(c, http.StatusNotFound, "ACCOUNT_NOT_FOUND", "账号不存在")
		return
	}

	if _, err := s.creationChannels(req.AccountID, req.Channel); err != nil {
		backendFail(c, err)
		return
	}
	// Serialize preflight with manual/API creation and other start requests.
	lock := s.aliasBatches.creationLock(req.AccountID)
	lock.Lock()
	defer lock.Unlock()
	s.aliasBatches.mu.Lock()
	previous := s.aliasBatches.jobs[req.AccountID]
	active := previous != nil && batchActive(previous.state.Status)
	s.aliasBatches.mu.Unlock()
	if active {
		failCode(c, http.StatusConflict, "BATCH_ALREADY_RUNNING", "该账号已有自动注册任务，请等待完成或先停止")
		return
	}
	total := 0
	if req.Channel == channelAuto {
		for _, a := range s.be.ListAccounts() {
			if a.ID == req.AccountID {
				total = a.AliasTotal
				break
			}
		}
	} else {
		aliases, err := s.be.ListAliases(req.AccountID)
		if err != nil {
			backendFail(c, err)
			return
		}
		total = len(aliases)
	}
	remaining := MaxAliases - total
	if !req.Continuous && remaining <= 0 {
		failCode(c, http.StatusConflict, "ALIAS_LIMIT_REACHED", "该账号别名总数已达到 750 个上限（含已停用别名）")
		return
	}
	if !req.Continuous && req.Count > remaining {
		failCode(c, http.StatusBadRequest, "ALIAS_QUOTA_EXCEEDED", fmt.Sprintf("该账号当前有 %d 个别名，本次最多注册 %d 个", total, remaining))
		return
	}
	if req.LabelPrefix == "" {
		req.LabelPrefix = "自动注册"
	}
	ctx, cancel := context.WithCancel(context.Background())
	job := &aliasBatchJob{
		state: aliasBatchStatus{
			Continuous: req.Continuous, ID: uuid.NewString(), AccountID: req.AccountID, Status: "running", Channel: req.Channel,
			Count: req.Count, IntervalSeconds: req.IntervalSeconds, LabelPrefix: req.LabelPrefix,
			CooldownMinutes: req.CooldownMinutes, BatchSize: aliasBatchSize,
			AliasTotal: total, MaxAliases: MaxAliases, StartedAt: time.Now().UTC(),
		},
		cancel: cancel,
	}
	s.aliasBatches.mu.Lock()
	if req.Channel == channelAuto {
		if !s.initializeDualLocked(job, req) {
			s.aliasBatches.mu.Unlock()
			cancel()
			failCode(c, 500, "SCHEDULE_SAVE_FAILED", "计划保存失败，未启动创建")
			return
		}
		job.done = make(chan struct{})
	}
	s.aliasBatches.jobs[req.AccountID] = job
	state := job.state
	s.aliasBatches.mu.Unlock()
	go s.runAliasBatch(ctx, job, req)
	c.JSON(http.StatusAccepted, apiResp{Success: true, Data: state})
}

func (s *Server) stopAliasBatchHandler(c *gin.Context) {
	var req struct {
		AccountID string `json:"account_id"`
		JobID     string `json:"job_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.AccountID == "" || req.JobID == "" {
		failCode(c, http.StatusBadRequest, "VALIDATION_ERROR", "account_id 和 job_id 必填")
		return
	}
	s.aliasBatches.mu.Lock()
	job := s.aliasBatches.jobs[req.AccountID]
	if job == nil || job.state.ID != req.JobID {
		s.aliasBatches.mu.Unlock()
		failCode(c, http.StatusNotFound, "BATCH_NOT_FOUND", "自动注册任务不存在或已被新任务替换")
		return
	}
	if batchActive(job.state.Status) {
		job.state.Status = "stopping"
		job.state.NextRunAt = nil
		job.cancel()
		if job.dual != nil && !s.checkpointDualLocked(job) {
			s.aliasBatches.mu.Unlock()
			failCode(c, 500, "SCHEDULE_SAVE_FAILED", "停止状态保存失败，请检查数据目录")
			return
		}
	}
	state := job.state
	s.aliasBatches.mu.Unlock()
	ok(c, state)
}

func (s *Server) finishAliasBatch(job *aliasBatchJob, status, code, message string) {
	s.aliasBatches.mu.Lock()
	defer s.aliasBatches.mu.Unlock()
	now := time.Now().UTC()
	job.state.Status = status
	job.state.ErrorCode = code
	job.state.Message = message
	job.state.FinishedAt = &now
	job.state.NextRunAt = nil
}

// Retry the same labeled item only after querying the authoritative directory.
// An upstream reservation may have succeeded even when its response was lost.
func (s *Server) createBatchAlias(ctx context.Context, accountID, label string, reconcile bool, channels ...string) (*hme.CreateResult, int, error) {
	lock := s.aliasBatches.creationLock(accountID)
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	aliases, err := s.be.ListAliases(accountID)
	if err != nil {
		return nil, 0, err
	}
	total := len(aliases)
	if reconcile {
		for _, alias := range aliases {
			if alias.Label == label && alias.Email != "" {
				return &hme.CreateResult{Email: alias.Email, Label: label, CreatedAt: alias.CreatedAt}, total, nil
			}
		}
	}
	if total >= MaxAliases {
		return nil, total, &BackendError{Status: http.StatusConflict, Code: "ALIAS_LIMIT_REACHED", Message: "该账号别名总数已达到 750 个上限（含已停用别名）"}
	}
	if err := ctx.Err(); err != nil {
		return nil, total, err
	}
	channel := channelWeb
	if len(channels) > 0 {
		channel = channels[0]
	}
	result, err := s.createOnChannel(ctx, accountID, label, channel)
	if err != nil {
		return nil, total, err
	}
	if result == nil || result.Email == "" {
		return nil, total, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", CreationChannel: channel, Message: "创建结果待确认"}
	}
	return result, total + 1, nil
}

// Back off from the task's configured delay, without shortening a longer user
// setting or Retry-After. This is a local schedule, not Apple's quota window.
func cooldownDelay(base time.Duration, failures int, retryAfter time.Duration) time.Duration {
	delay := base
	ceiling := max(base, 2*time.Hour)
	for i := 1; i < failures && delay < ceiling; i++ {
		delay *= 2
	}
	if delay > ceiling {
		delay = ceiling
	}
	if retryAfter > delay {
		delay = retryAfter
	}
	return delay
}

func (s *Server) waitAliasBatch(ctx context.Context, job *aliasBatchJob, delay time.Duration, cooling bool, code, message string) bool {
	next := time.Now().UTC().Add(delay)
	s.aliasBatches.mu.Lock()
	if ctx.Err() != nil || job.state.Status == "stopping" {
		s.aliasBatches.mu.Unlock()
		s.finishAliasBatch(job, "stopped", "", "已停止自动注册")
		return false
	}
	job.state.NextRunAt = &next
	if cooling {
		job.state.Status = "cooling_down"
		job.state.CooldownCount++
		job.state.ErrorCode = code
		job.state.Message = message + "；冷却结束后自动核对别名并继续未完成任务"
	}
	s.aliasBatches.mu.Unlock()
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		s.finishAliasBatch(job, "stopped", "", "已停止自动注册")
		return false
	case <-timer.C:
	}
	s.aliasBatches.mu.Lock()
	stopped := ctx.Err() != nil || job.state.Status == "stopping"
	if !stopped {
		job.state.NextRunAt = nil
		job.state.Status = "running"
		job.state.ErrorCode = ""
		job.state.Message = ""
	}
	s.aliasBatches.mu.Unlock()
	if stopped {
		s.finishAliasBatch(job, "stopped", "", "已停止自动注册")
	}
	return !stopped
}

func (s *Server) runAliasBatch(ctx context.Context, job *aliasBatchJob, req aliasBatchRequest) {
	if req.Channel == channelAuto {
		s.runDualBatch(ctx, job)
		return
	}
	if req.Channel == channelApple {
		s.runChannelBatch(ctx, job, req)
		return
	}
	defer job.cancel()
	interval := time.Duration(req.IntervalSeconds * float64(time.Second))
	cooldown := time.Duration(req.CooldownMinutes) * s.aliasBatches.minuteDuration
	failures := 0
	for i := 0; i < req.Count; {
		// Full task UUID avoids label collisions across distinct batches.
		label := fmt.Sprintf("%s-%s-%03d", req.LabelPrefix, job.state.ID, i+1)
		result, total, err := s.createBatchAlias(ctx, req.AccountID, label, failures > 0)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				s.finishAliasBatch(job, "stopped", "", "已停止自动注册")
				return
			}
			be := asBackendError(err)
			if be.Code == "UPSTREAM_FAILURE" || be.Code == "UPSTREAM_RATE_LIMITED" || be.Code == "RATE_LIMITED" {
				failures++
				if !s.waitAliasBatch(ctx, job, cooldownDelay(cooldown, failures, be.RetryAfter), true, be.Code, be.Message) {
					return
				}
				continue // Do not consume an item from the requested count.
			}
			status := "failed"
			if be.Code == "ALIAS_LIMIT_REACHED" {
				status = "limit_reached"
				s.aliasBatches.mu.Lock()
				job.state.AliasTotal = total
				s.aliasBatches.mu.Unlock()
			}
			s.finishAliasBatch(job, status, be.Code, be.Message)
			return
		}
		i++
		failures = 0
		s.aliasBatches.mu.Lock()
		job.state.Created++
		job.state.AliasTotal = total
		job.state.LastEmail = result.Email
		job.state.ErrorCode = ""
		job.state.Message = ""
		s.aliasBatches.mu.Unlock()
		if i == req.Count {
			s.finishAliasBatch(job, "completed", "", "本次自动注册已完成")
			return
		}
		// Do not send a sixth request to discover the upstream limit. Keep the
		// completed count and resume this same task after the configured pause.
		if i%aliasBatchSize == 0 {
			if !s.waitAliasBatch(ctx, job, max(interval, cooldown), true, "", fmt.Sprintf("本轮已创建 %d 个，按设置冷却 %d 分钟", aliasBatchSize, req.CooldownMinutes)) {
				return
			}
		} else if !s.waitAliasBatch(ctx, job, interval, false, "", "") {
			return
		}
	}
}

// Shutdown preserves durable auto schedules, but never resumes an uncertain call.
func (s *Server) Close() {
	m := s.aliasBatches
	m.mu.Lock()
	m.closing = true
	var done []chan struct{}
	for _, job := range m.jobs {
		if batchActive(job.state.Status) {
			if job.dual == nil {
				job.state.Status = "stopping"
				job.state.NextRunAt = nil
			}
			job.cancel()
		}
		if job.done != nil {
			done = append(done, job.done)
		}
	}
	m.mu.Unlock()
	for _, ch := range done {
		<-ch
	}
}
