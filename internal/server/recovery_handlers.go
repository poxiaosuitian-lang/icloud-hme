package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type recoveryWaitRequest struct {
	Version  int      `json:"version"`
	ClientID string   `json:"client_id"`
	Sources  []string `json:"sources"`
}
type recoveryCompleteRequest struct {
	Version  int    `json:"version"`
	ClientID string `json:"client_id"`
	JobID    string `json:"job_id"`
	Lease    string `json:"lease"`
	Outcome  string `json:"outcome"`
}

// Unlike Allow(), successful long polls must NEVER accumulate failed-login
// counts (nor consume importSlots reserved for real authentication work).
func (s *Server) authorizeRecovery(c *gin.Context) bool {
	if s.cfg.ImportToken == "" {
		importFailure(c, 404, "IMPORT_DISABLED")
		return false
	}
	headers := c.Request.Header.Values("Authorization")
	value := ""
	if len(headers) == 1 && strings.HasPrefix(headers[0], "Bearer ") {
		value = strings.TrimPrefix(headers[0], "Bearer ")
	}
	expected, actual := sha256.Sum256([]byte(s.cfg.ImportToken)), sha256.Sum256([]byte(value))
	if value == "" || subtle.ConstantTimeCompare(expected[:], actual[:]) != 1 {
		status := 401
		code := "IMPORT_UNAUTHORIZED"
		if allowed, _ := s.recoveryLimiter.Allow(c.ClientIP()); !allowed {
			status = 429
			code = "IMPORT_RATE_LIMITED"
		}
		importFailure(c, status, code)
		return false
	}
	s.recoveryLimiter.Success(c.ClientIP())
	return true
}
func decodeRecovery(c *gin.Context, out any, fields ...string) bool {
	media, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || media != "application/json" || c.Request.URL.RawQuery != "" || c.GetHeader("Content-Encoding") != "" {
		return false
	}
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 8192))
	defer clear(raw)
	if err != nil || !uniqueImportKeys(raw) {
		return false
	}
	var shape map[string]json.RawMessage
	if json.Unmarshal(raw, &shape) != nil || len(shape) != len(fields) {
		return false
	}
	for _, key := range fields {
		v, ok := shape[key]
		if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return false
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(out) == nil
}
func (s *Server) recoveryWait(c *gin.Context) {
	if !s.authorizeRecovery(c) {
		return
	}
	var req recoveryWaitRequest
	if !decodeRecovery(c, &req, "version", "client_id", "sources") || req.Version != 1 || !recoveryClientPattern.MatchString(req.ClientID) || len(req.Sources) == 0 || len(req.Sources) > 50 {
		importFailure(c, 400, "INVALID_RECOVERY_REQUEST")
		return
	}
	seen := map[string]bool{}
	for _, source := range req.Sources {
		if !recoverySourcePattern.MatchString(source) || seen[source] {
			importFailure(c, 400, "INVALID_RECOVERY_REQUEST")
			return
		}
		seen[source] = true
	}
	if _, ok := s.be.(recoveryBackend); !ok {
		importFailure(c, 503, "RECOVERY_UNAVAILABLE")
		return
	}
	if _, err := s.recoverySources(req.Sources); err != nil {
		importFailure(c, 409, "RECOVERY_SOURCE_CONFLICT")
		return
	}
	q := s.recovery
	select {
	case q.waitSlots <- struct{}{}:
		defer func() { <-q.waitSlots }()
	default:
		importFailure(c, 503, "RECOVERY_BUSY")
		return
	}
	q.mu.Lock()
	for _, source := range req.Sources {
		q.waiting[source]++
		q.lastSeen[source] = q.now().UTC()
	}
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		defer q.mu.Unlock()
		for _, source := range req.Sources {
			q.waiting[source]--
			q.lastSeen[source] = q.now().UTC()
		}
	}()
	ctx, cancel := context.WithTimeout(c.Request.Context(), q.waitDuration)
	defer cancel()
	for {
		if ctx.Err() != nil {
			if c.Request.Context().Err() == nil {
				ok(c, gin.H{"job": nil})
			}
			return
		}
		job, notify, delay, err := s.claimRecovery(req.ClientID, req.Sources)
		if err != nil {
			code := "RECOVERY_SOURCE_CONFLICT"
			status := 409
			if err == errRecoveryStorage {
				status = 503
				code = "RECOVERY_STORAGE_FAILED"
			}
			importFailure(c, status, code)
			return
		}
		if job != nil {
			ok(c, gin.H{"job": job})
			return
		}
		timer := time.NewTimer(max(delay, time.Millisecond))
		select {
		case <-ctx.Done():
		case <-notify:
		case <-timer.C:
		}
		timer.Stop()
	}
}
func sameRecoveryLease(r recoveryRecord, req recoveryCompleteRequest) bool {
	expected, actual := sha256.Sum256([]byte(r.Lease)), sha256.Sum256([]byte(req.Lease))
	return r.ID == req.JobID && r.Client == req.ClientID && subtle.ConstantTimeCompare(expected[:], actual[:]) == 1
}
func (s *Server) recoveryComplete(c *gin.Context) {
	if !s.authorizeRecovery(c) {
		return
	}
	var req recoveryCompleteRequest
	if !decodeRecovery(c, &req, "version", "client_id", "job_id", "lease", "outcome") || req.Version != 1 || !recoveryClientPattern.MatchString(req.ClientID) || !recoveryIDPattern.MatchString(req.JobID) || !recoveryLeasePattern.MatchString(req.Lease) {
		importFailure(c, 400, "INVALID_RECOVERY_REQUEST")
		return
	}
	switch req.Outcome {
	case "ready", "verification_required", "failed", "local_unavailable":
	default:
		importFailure(c, 400, "INVALID_RECOVERY_REQUEST")
		return
	}
	q := s.recovery
	select {
	case q.completeSlots <- struct{}{}:
		defer func() { <-q.completeSlots }()
	default:
		importFailure(c, 503, "RECOVERY_BUSY")
		return
	}
	q.mu.Lock()
	var r recoveryRecord
	found := false
	for _, record := range q.records {
		if record.ID == req.JobID {
			r = record
			found = true
			break
		}
	}
	current, valid := s.recoveryBinding(r.Account.AccountID)
	if !found || !valid || !recoveryMatches(r, current) || !sameRecoveryLease(r, req) || !q.now().Before(r.LeaseUntil) {
		q.mu.Unlock()
		importFailure(c, 409, "RECOVERY_LEASE_CONFLICT")
		return
	}
	if r.Outcome != "" {
		q.mu.Unlock()
		if r.Outcome != req.Outcome {
			importFailure(c, 409, "RECOVERY_LEASE_CONFLICT")
			return
		}
		if r.Outcome == "ready" {
			s.applyRecoveryReady(r)
		}
		c.JSON(200, gin.H{"success": true})
		return
	}
	if r.State != "leased" || q.verifying[r.ID] {
		q.mu.Unlock()
		importFailure(c, 409, "RECOVERY_BUSY")
		return
	}
	q.verifying[r.ID] = true
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		delete(q.verifying, r.ID)
		close(q.notify)
		q.notify = make(chan struct{})
		q.mu.Unlock()
	}()
	if req.Outcome == "ready" {
		b, okBackend := s.be.(recoveryBackend)
		if !okBackend {
			importFailure(c, 503, "RECOVERY_UNAVAILABLE")
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
		err := b.VerifyRecovery(ctx, current)
		cancel()
		if err != nil {
			importFailure(c, 409, "RECOVERY_NOT_READY")
			return
		}
	}
	q.mu.Lock()
	latest := q.records[r.Account.AccountID]
	after, valid := s.recoveryBinding(r.Account.AccountID)
	if !valid || after != current || !recoveryMatches(latest, after) || !sameRecoveryLease(latest, req) || latest.Outcome != "" || !q.now().Before(latest.LeaseUntil) {
		q.mu.Unlock()
		importFailure(c, 409, "RECOVERY_LEASE_CONFLICT")
		return
	}
	latest.Outcome = req.Outcome
	switch req.Outcome {
	case "ready":
		latest.State = "done"
		latest.AcceptedSession = current.Session
	case "verification_required":
		latest.State = "paused"
	case "local_unavailable":
		latest.State = "pending"
		latest.Due = q.now().Add(5 * time.Minute)
	case "failed":
		latest.Failures++
		latest.State = "pending"
		latest.Due = q.now().Add(5 * time.Minute)
		if latest.Failures == 2 {
			latest.Due = q.now().Add(15 * time.Minute)
		}
		if latest.Failures >= 3 {
			latest.State = "exhausted"
		}
	}
	next := q.copy()
	next[r.Account.AccountID] = latest
	err := q.save(next)
	q.mu.Unlock()
	if err != nil {
		importFailure(c, 503, "RECOVERY_STORAGE_FAILED")
		return
	}
	if req.Outcome == "ready" {
		s.applyRecoveryReady(latest)
	}
	c.JSON(200, gin.H{"success": true})
}

var recoveryMessages = map[string]string{"pending": "等待本机协助", "leased": "本机正在恢复登录", "paused": "需在 Windows 面板完成验证", "exhausted": "自动恢复重试已达上限，请在 Windows 面板手动登录", "done": "会话已验证，运行中的任务将在下一正常时隙恢复", "cancelled": "账号或会话已更新，旧恢复任务已失效"}

// Local-only operational view: never probes Apple or returns job/lease secrets.
func (s *Server) recoveryStatus(c *gin.Context) {
	q := s.recovery
	q.mu.Lock()
	defer q.mu.Unlock()
	rows := make([]gin.H, 0)
	for _, a := range s.be.ListAccounts() {
		v, valid := s.recoveryBinding(a.ID)
		if !valid {
			continue
		}
		state := "idle"
		message := "无恢复需求，仅在创建时触发"
		if r, exists := q.records[a.ID]; exists {
			state = r.State
			if !recoveryMatches(r, v) {
				state = "cancelled"
			}
			message = recoveryMessages[state]
		}
		rows = append(rows, gin.H{"account_id": a.ID, "state": state, "message": message, "waiting_connections": q.waiting[v.SourceID], "last_wait_at": q.lastSeen[v.SourceID]})
	}
	ok(c, gin.H{"accounts": rows, "waiting_connections": len(q.waitSlots)})
}
