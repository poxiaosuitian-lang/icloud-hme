package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"icloud-hme/internal/account"
)

var recoverySourcePattern = regexp.MustCompile(`^appleservices-[a-f0-9]{64}$`)
var recoveryClientPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var recoveryLeasePattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var recoveryIDPattern = regexp.MustCompile(`^job_[A-Za-z0-9_-]{1,124}$`)
var errRecoveryConflict = errors.New("RECOVERY_CONFLICT")
var errRecoveryStorage = errors.New("RECOVERY_STORAGE_FAILED")

type recoveryBackend interface {
	RecoveryBinding(string) (account.RecoveryBinding, bool)
	VerifyRecovery(context.Context, account.RecoveryBinding) error
}

func (b *managerBackend) RecoveryBinding(id string) (account.RecoveryBinding, bool) {
	return b.mgr.RecoveryBinding(id)
}
func (b *managerBackend) VerifyRecovery(ctx context.Context, v account.RecoveryBinding) error {
	return b.mgr.VerifyRecovery(ctx, v)
}

type recoveryRecord struct {
	Kind            string                  `json:"kind,omitempty"`
	ID              string                  `json:"id"`
	Account         account.RecoveryBinding `json:"account"`
	AcceptedSession string                  `json:"accepted_session,omitempty"`
	State           string                  `json:"state"`
	Lease           string                  `json:"lease,omitempty"`
	Client          string                  `json:"client,omitempty"`
	LeaseUntil      time.Time               `json:"lease_until,omitempty"`
	Due             time.Time               `json:"due"`
	Failures        int                     `json:"failures"`
	Outcome         string                  `json:"outcome,omitempty"`
}
type recoveryJob struct {
	ID       string `json:"id"`
	Lease    string `json:"lease"`
	SourceID string `json:"source_id"`
	Kind     string `json:"kind"`
}
type recoveryQueue struct {
	mu            sync.Mutex
	path          string
	records       map[string]recoveryRecord // One record per account; terminal fences retained.
	notify        chan struct{}
	now           func() time.Time
	waitDuration  time.Duration
	waitSlots     chan struct{}
	completeSlots chan struct{}
	verifying     map[string]bool
	waiting       map[string]int
	lastSeen      map[string]time.Time
}

func newRecoveryQueue(path string) *recoveryQueue {
	return &recoveryQueue{path: path, records: map[string]recoveryRecord{}, notify: make(chan struct{}), now: time.Now, waitDuration: 25 * time.Second, waitSlots: make(chan struct{}, 32), completeSlots: make(chan struct{}, 4), verifying: map[string]bool{}, waiting: map[string]int{}, lastSeen: map[string]time.Time{}}
}
func recoveryNonce() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func (q *recoveryQueue) load() error {
	if q.path == "" {
		return nil
	}
	raw, err := os.ReadFile(q.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return errRecoveryStorage
	}
	var doc struct {
		Version int                       `json:"version"`
		Records map[string]recoveryRecord `json:"records"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.Version != 1 || doc.Records == nil {
		return errRecoveryStorage
	}
	ids := map[string]bool{}
	for id, r := range doc.Records {
		if !validRecoveryKind(r.Kind) || id != recoveryRecordKey(r.Account.AccountID, r.Kind) || !recoveryIDPattern.MatchString(r.ID) || ids[r.ID] || !recoverySourcePattern.MatchString(r.Account.SourceID) || !recoveryLeasePattern.MatchString(r.Account.Binding) || r.Account.Session == "" || r.Failures < 0 || r.Failures > 3 {
			return errRecoveryStorage
		}
		ids[r.ID] = true
		switch r.State {
		case "pending", "leased", "paused", "exhausted", "done", "cancelled":
		default:
			return errRecoveryStorage
		}
		if r.Lease != "" && (!recoveryLeasePattern.MatchString(r.Lease) || !recoveryClientPattern.MatchString(r.Client) || r.LeaseUntil.IsZero()) {
			return errRecoveryStorage
		}
		if r.State == "leased" && r.Lease == "" {
			return errRecoveryStorage
		}
		if r.State == "done" && (r.Outcome != "ready" || r.AcceptedSession == "") ||
			r.State == "paused" && r.Outcome != "verification_required" ||
			r.State == "exhausted" && (r.Outcome != "failed" || r.Failures != 3) ||
			r.State == "leased" && r.Outcome != "" ||
			r.State == "pending" && (r.Due.IsZero() || (r.Outcome != "" && r.Outcome != "failed" && r.Outcome != "local_unavailable")) {
			return errRecoveryStorage
		}
	}
	if os.Chmod(q.path, 0600) != nil {
		return errRecoveryStorage
	}
	q.records = doc.Records
	return nil
}

// Disk commit before publication. No network operations while mu is held.
func (q *recoveryQueue) save(next map[string]recoveryRecord) error {
	if q.path != "" {
		raw, err := json.Marshal(struct {
			Version int                       `json:"version"`
			Records map[string]recoveryRecord `json:"records"`
		}{1, next})
		if err != nil {
			return errRecoveryStorage
		}
		if os.MkdirAll(filepath.Dir(q.path), 0700) != nil {
			return errRecoveryStorage
		}
		f, err := os.CreateTemp(filepath.Dir(q.path), ".login-recovery-*")
		if err != nil {
			return errRecoveryStorage
		}
		defer os.Remove(f.Name())
		if err = f.Chmod(0600); err == nil {
			_, err = f.Write(raw)
		}
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			return errRecoveryStorage
		}
		if os.Rename(f.Name(), q.path) != nil {
			return errRecoveryStorage
		}
		if d, e := os.Open(filepath.Dir(q.path)); e == nil {
			_ = d.Sync()
			_ = d.Close()
		}
	}
	q.records = next
	close(q.notify)
	q.notify = make(chan struct{})
	return nil
}
func (q *recoveryQueue) copy() map[string]recoveryRecord {
	out := make(map[string]recoveryRecord, len(q.records))
	for id, r := range q.records {
		out[id] = r
	}
	return out
}
func (s *Server) recoveryBinding(id string) (account.RecoveryBinding, bool) {
	b, ok := s.be.(recoveryBackend)
	if !ok {
		return account.RecoveryBinding{}, false
	}
	return b.RecoveryBinding(id)
}
func recoveryMatches(r recoveryRecord, current account.RecoveryBinding) bool {
	return r.Account.AccountID == current.AccountID && r.Account.SourceID == current.SourceID && r.Account.Binding == current.Binding && (r.Account.Session == current.Session || r.AcceptedSession != "" && r.AcceptedSession == current.Session)
}

// Called only on a creation event, with the version observed BEFORE the failed
// operation. A late failure never queues recovery for a newer successful login.
func (s *Server) requestRecovery(observed account.RecoveryBinding) {
	s.requestRecoveryKind(observed, "apple_login")
}
func (s *Server) requestRecoveryKind(observed account.RecoveryBinding, kind string) {
	q := s.recovery
	if q == nil || observed.AccountID == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	current, ok := s.recoveryBindingKind(observed.AccountID, kind)
	if !ok || current != observed {
		return
	}
	key := recoveryRecordKey(current.AccountID, kind)
	if old, exists := q.records[key]; exists && old.Account == current && old.State != "cancelled" {
		return
	}
	nonce, err := recoveryNonce()
	if err != nil {
		return
	}
	next := q.copy()
	next[key] = recoveryRecord{Kind: kind, ID: "job_" + nonce, Account: current, State: "pending", Due: q.now().UTC()}
	_ = q.save(next) // Creation still fails/downgrades if persistence fails; never lease uncommitted work.
}

// Sources grant access only to existing verified imports, not arbitrary account
// IDs. Recheck on EVERY wakeup and complete, including after readonly validation.
func (s *Server) recoverySources(sources []string) (map[string]account.RecoveryBinding, error) {
	bySource := map[string]account.RecoveryBinding{}
	for _, a := range s.be.ListAccounts() {
		if v, ok := s.recoveryBinding(a.ID); ok {
			if _, dup := bySource[v.SourceID]; dup {
				return nil, errRecoveryConflict
			}
			bySource[v.SourceID] = v
		}
	}
	selected := map[string]account.RecoveryBinding{}
	for _, source := range sources {
		v, ok := bySource[source]
		if !ok {
			return nil, errRecoveryConflict
		}
		selected[v.AccountID] = v
	}
	return selected, nil
}
func (s *Server) claimRecovery(client string, sources []string, kinds ...string) (*recoveryJob, <-chan struct{}, time.Duration, error) {
	q := s.recovery
	q.mu.Lock()
	defer q.mu.Unlock()
	selected, err := s.recoverySources(sources)
	if err != nil {
		return nil, q.notify, 0, err
	}
	if len(kinds) == 0 {
		kinds = []string{"apple_login"}
	}
	expanded := map[string]account.RecoveryBinding{}
	for id, binding := range selected {
		for _, kind := range kinds {
			if kind == "web_session" {
				if web, ok := s.recoveryBindingKind(id, kind); ok {
					expanded[recoveryRecordKey(id, kind)] = web
				}
			} else {
				expanded[id] = binding
			}
		}
	}
	selected = expanded
	now := q.now().UTC()
	next := q.copy()
	changed := false
	delay := q.waitDuration
	ids := make([]string, 0, len(selected))
	for id := range selected {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var job *recoveryJob
	for _, id := range ids {
		r, exists := next[id]
		if !exists {
			continue
		}
		if !recoveryMatches(r, selected[id]) {
			if r.State != "cancelled" {
				r.State = "cancelled"
				next[id] = r
				changed = true
			}
			continue
		}
		if q.verifying[r.ID] {
			continue
		}
		switch r.State {
		case "pending", "leased":
		default:
			continue
		}
		due := r.Due
		if r.State == "leased" {
			due = r.LeaseUntil
		}
		if now.Before(due) {
			delay = min(delay, due.Sub(now))
			continue
		}
		if job != nil {
			continue
		}
		// Failed attempts get a new job ID only after bounded explicit backoff.
		// Lost leases and local_unavailable always redeliver the SAME ID.
		if r.Outcome == "failed" {
			nonce, e := recoveryNonce()
			if e != nil {
				return nil, q.notify, 0, errRecoveryStorage
			}
			r.ID = "job_" + nonce
		}
		lease, e := recoveryNonce()
		if e != nil {
			return nil, q.notify, 0, errRecoveryStorage
		}
		r.State = "leased"
		r.Lease = lease
		r.Client = client
		r.LeaseUntil = now.Add(300 * time.Second)
		r.Outcome = ""
		next[id] = r
		changed = true
		job = &recoveryJob{r.ID, lease, r.Account.SourceID, normalizedRecoveryKind(r.Kind)}
	}
	if changed {
		if err = q.save(next); err != nil {
			return nil, q.notify, 0, err
		}
	}
	return job, q.notify, delay, nil
}

// Successful integration login/status may have saved a new session while a
// lease is active. Only this existing trusted server endpoint can authorize that
// transition; browser/manual edits simply invalidate the old lease by version.
func (s *Server) acceptRecoverySession(source string) {
	q := s.recovery
	q.mu.Lock()
	defer q.mu.Unlock()
	next := q.copy()
	changed := false
	for id, r := range next {
		if normalizedRecoveryKind(r.Kind) != "apple_login" {
			continue
		}
		if r.Account.SourceID != source || r.State != "leased" || !q.now().Before(r.LeaseUntil) {
			continue
		}
		v, ok := s.recoveryBinding(id)
		if ok && v.Binding == r.Account.Binding {
			r.AcceptedSession = v.Session
			next[id] = r
			changed = true
		}
	}
	if changed {
		_ = q.save(next)
	}
}

// Only clear the rejection for the failed revision, never change task state,
// deadlines, budget, counts or stop flags. Replayed ready confirmations are safe.
func (s *Server) applyRecoveryReady(r recoveryRecord) {
	if normalizedRecoveryKind(r.Kind) == "web_session" {
		return
	} // Web scheduler uses cookie/validation revision on its next normal slot.
	current, ok := s.recoveryBinding(r.Account.AccountID)
	if !ok || !recoveryMatches(r, current) {
		return
	}
	m := s.aliasBatches
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[r.Account.AccountID]
	if job == nil || job.dual == nil || !batchActive(job.state.Status) {
		return
	}
	if job.dual.AppleRejected && job.dual.AppleRevision == r.Account.Session {
		job.dual.AppleRejected = false
		job.dual.AppleRevision = current.Session
		s.checkpointDualLocked(job)
	}
}
