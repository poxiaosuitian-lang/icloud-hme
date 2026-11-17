package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
)

func normalizedRecoveryKind(kind string) string {
	if kind == "" {
		return "apple_login"
	}
	return kind
}
func validRecoveryKind(kind string) bool {
	return kind == "" || kind == "apple_login" || kind == "web_session"
}
func recoveryRecordKey(id, kind string) string {
	if kind == "web_session" {
		return id + ":web"
	}
	return id
}

type webRecoveryBackend interface {
	WebRecoveryBinding(string) (account.RecoveryBinding, bool)
	PrepareWebRecovery(context.Context, account.RecoveryBinding, account.WebRecoveryRequest) (*account.WebRecoveryUpdate, error)
	CommitWebRecovery(*account.WebRecoveryUpdate) error
	VerifyWebRecovery(context.Context, account.RecoveryBinding) error
}

func (b *managerBackend) WebRecoveryBinding(id string) (account.RecoveryBinding, bool) {
	return b.mgr.WebRecoveryBinding(id)
}
func (b *managerBackend) PrepareWebRecovery(ctx context.Context, v account.RecoveryBinding, r account.WebRecoveryRequest) (*account.WebRecoveryUpdate, error) {
	return b.mgr.PrepareWebRecovery(ctx, v, r)
}
func (b *managerBackend) CommitWebRecovery(v *account.WebRecoveryUpdate) error {
	return b.mgr.CommitWebRecovery(v)
}
func (b *managerBackend) VerifyWebRecovery(ctx context.Context, v account.RecoveryBinding) error {
	return b.mgr.VerifyWebRecovery(ctx, v)
}
func (s *Server) recoveryBindingKind(id, kind string) (account.RecoveryBinding, bool) {
	if kind == "web_session" {
		if b, ok := s.be.(webRecoveryBackend); ok {
			return b.WebRecoveryBinding(id)
		}
		return account.RecoveryBinding{}, false
	}
	return s.recoveryBinding(id)
}

// Old Windows clients advertise no kinds and receive only Apple Account jobs.
func decodeRecoveryWait(c *gin.Context, out *recoveryWaitRequest) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 8192))
	defer clear(raw)
	if err != nil {
		return false
	}
	var shape map[string]json.RawMessage
	if json.Unmarshal(raw, &shape) != nil {
		return false
	}
	fields := []string{"version", "client_id", "sources"}
	if _, ok := shape["kinds"]; ok {
		fields = append(fields, "kinds")
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
	if !decodeRecovery(c, out, fields...) {
		return false
	}
	if len(fields) == 4 && (len(out.Kinds) == 0 || len(out.Kinds) > 2) {
		return false
	}
	seen := map[string]bool{}
	for _, kind := range out.Kinds {
		if kind == "" || !validRecoveryKind(kind) || seen[kind] {
			return false
		}
		seen[kind] = true
	}
	return true
}

func (s *Server) noteWebRecovery(observed account.RecoveryBinding, err error) {
	if err == nil {
		return
	}
	code := asBackendError(err).Code
	if code == "UPSTREAM_UNAUTHORIZED" || code == "COOKIES_REQUIRED" {
		s.requestRecoveryKind(observed, "web_session")
	}
}
func (s *Server) listAliasesWithRecovery(id string) ([]hme.Alias, error) {
	observed, _ := s.recoveryBindingKind(id, "web_session")
	aliases, err := s.be.ListAliases(id)
	s.noteWebRecovery(observed, err)
	return aliases, err
}

func (s *Server) recoveryWebSession(c *gin.Context) {
	if !s.authorizeRecovery(c) {
		return
	}
	var req account.WebRecoveryRequest
	if !decodeRecovery(c, &req, "version", "client_id", "job_id", "lease", "source_id", "apple_id", "host", "cookies") || req.Version != 1 || !recoveryClientPattern.MatchString(req.ClientID) || !recoveryIDPattern.MatchString(req.JobID) || !recoveryLeasePattern.MatchString(req.Lease) {
		importFailure(c, 400, "INVALID_RECOVERY_REQUEST")
		return
	}
	b, ok := s.be.(webRecoveryBackend)
	if !ok {
		importFailure(c, 503, "RECOVERY_UNAVAILABLE")
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
	lease := recoveryCompleteRequest{Version: 1, ClientID: req.ClientID, JobID: req.JobID, Lease: req.Lease, Outcome: "ready"}
	q.mu.Lock()
	var r recoveryRecord
	for _, record := range q.records {
		if record.ID == req.JobID {
			r = record
			break
		}
	}
	current, valid := s.recoveryBindingKind(r.Account.AccountID, "web_session")
	if r.Kind != "web_session" || !valid || req.SourceID != r.Account.SourceID || !recoveryMatches(r, current) || !sameRecoveryLease(r, lease) || !q.now().Before(r.LeaseUntil) {
		q.mu.Unlock()
		importFailure(c, 409, "RECOVERY_LEASE_CONFLICT")
		return
	}
	if r.State == "done" && r.Outcome == "ready" {
		q.mu.Unlock()
		okWebRecovery(c, r.Account.SourceID)
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
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	update, err := b.PrepareWebRecovery(ctx, current, req)
	clear(req.Cookies)
	if err != nil {
		importFailure(c, 422, "WEB_RECOVERY_VERIFICATION_FAILED")
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	key := recoveryRecordKey(r.Account.AccountID, r.Kind)
	latest := q.records[key]
	after, valid := s.recoveryBindingKind(r.Account.AccountID, r.Kind)
	if !valid || after != current || !sameRecoveryLease(latest, lease) || latest.State != "leased" || latest.Outcome != "" || !q.now().Before(latest.LeaseUntil) || ctx.Err() != nil {
		importFailure(c, 409, "RECOVERY_LEASE_CONFLICT")
		return
	}
	if b.CommitWebRecovery(update) != nil {
		importFailure(c, 409, "WEB_RECOVERY_COMMIT_FAILED")
		return
	}
	accepted, valid := s.recoveryBindingKind(r.Account.AccountID, r.Kind)
	if !valid || accepted.Binding != r.Account.Binding {
		importFailure(c, 409, "RECOVERY_LEASE_CONFLICT")
		return
	}
	latest.AcceptedSession = accepted.Session
	latest.State = "done"
	latest.Outcome = "ready"
	next := q.copy()
	next[key] = latest
	if q.save(next) != nil {
		importFailure(c, 503, "RECOVERY_STORAGE_FAILED")
		return
	}
	okWebRecovery(c, r.Account.SourceID)
}
func okWebRecovery(c *gin.Context, source string) {
	ok(c, gin.H{"source_id": source, "state": "ready", "web_verified": true})
}
