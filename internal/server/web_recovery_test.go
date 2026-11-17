package server

import (
	"context"
	"encoding/json"
	"icloud-hme/internal/account"
	"strings"
	"testing"
)

type webRecoveryFake struct {
	*recoveryFake
	prepares, commits int
	hook              func()
	fail              bool
}

func (b *webRecoveryFake) WebRecoveryBinding(id string) (account.RecoveryBinding, bool) {
	v, ok := b.RecoveryBinding(id)
	v.Session = "web:" + v.Session
	return v, ok
}
func (b *webRecoveryFake) PrepareWebRecovery(context.Context, account.RecoveryBinding, account.WebRecoveryRequest) (*account.WebRecoveryUpdate, error) {
	b.prepares++
	if b.hook != nil {
		b.hook()
	}
	if b.fail {
		return nil, account.ImportConflict
	}
	return &account.WebRecoveryUpdate{}, nil
}
func (b *webRecoveryFake) CommitWebRecovery(*account.WebRecoveryUpdate) error {
	b.commits++
	return nil
}
func (b *webRecoveryFake) VerifyWebRecovery(context.Context, account.RecoveryBinding) error {
	return nil
}

func TestWebRecoveryLeaseCookieReturnAndReplay(t *testing.T) {
	s, base, _ := recoveryFixture(t)
	b := &webRecoveryFake{recoveryFake: base}
	s.be = b
	v, _ := b.WebRecoveryBinding("one")
	for i := 0; i < 3; i++ {
		s.noteWebRecovery(v, &BackendError{Code: "UPSTREAM_UNAUTHORIZED"})
	}
	s.requestRecovery(base.bindings["one"])
	if len(s.recovery.records) != 2 {
		t.Fatal("channels did not isolate demand")
	}
	old := waitBody(base, "one")
	w := recoveryCall(s, "wait", old)
	if !strings.Contains(w.Body.String(), `"kind":"apple_login"`) {
		t.Fatal("old client received web task")
	}
	req := old
	req.Kinds = []string{"web_session"}
	w = recoveryCall(s, "wait", req)
	var envelope struct {
		Data struct {
			Job recoveryJob `json:"job"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &envelope)
	job := envelope.Data.Job
	if job.Kind != "web_session" {
		t.Fatal(w.Body.String())
	}
	body := account.WebRecoveryRequest{Version: 1, ClientID: req.ClientID, JobID: job.ID, Lease: job.Lease, SourceID: v.SourceID, AppleID: "one@example.test", Host: "icloud.com", Cookies: map[string]string{"fixture": "cookie-secret"}}
	bad := body
	bad.SourceID = base.bindings["two"].SourceID
	if recoveryCall(s, "web-session", bad).Code != 409 || b.prepares != 0 {
		t.Fatal("cross-account accepted")
	}
	for i := 0; i < 2; i++ {
		w = recoveryCall(s, "web-session", body)
		if w.Code != 200 || strings.Contains(w.Body.String(), "cookie-secret") {
			t.Fatal("cookie return failed or leaked")
		}
	}
	if b.prepares != 1 || b.commits != 1 {
		t.Fatal("replayed credential update")
	}
	complete := recoveryCompleteRequest{Version: 1, ClientID: req.ClientID, JobID: job.ID, Lease: job.Lease, Outcome: "ready"}
	if recoveryCall(s, "complete", complete).Code != 200 {
		t.Fatal("ready acknowledgement failed")
	}
}
func TestWebRecoveryRejectsRevokedLeaseBeforeCommit(t *testing.T) {
	s, base, _ := recoveryFixture(t)
	b := &webRecoveryFake{recoveryFake: base}
	s.be = b
	v, _ := b.WebRecoveryBinding("one")
	s.noteWebRecovery(v, &BackendError{Code: "UPSTREAM_UNAUTHORIZED"})
	job, _, _, err := s.claimRecovery(strings.Repeat("b", 32), []string{v.SourceID}, "web_session")
	if err != nil || job == nil {
		t.Fatal("no lease")
	}
	b.hook = func() {
		s.recovery.mu.Lock()
		r := s.recovery.records["one:web"]
		r.State = "cancelled"
		s.recovery.records["one:web"] = r
		s.recovery.mu.Unlock()
	}
	body := account.WebRecoveryRequest{Version: 1, ClientID: strings.Repeat("b", 32), JobID: job.ID, Lease: job.Lease, SourceID: v.SourceID, AppleID: "one@example.test", Host: "icloud.com", Cookies: map[string]string{"fixture": "secret"}}
	if recoveryCall(s, "web-session", body).Code != 409 || b.commits != 0 {
		t.Fatal("revoked lease committed")
	}
}
func TestWebRecoveryIgnoresLegacyAndNonAuthFailures(t *testing.T) {
	s, base, _ := recoveryFixture(t)
	b := &webRecoveryFake{recoveryFake: base}
	s.be = b
	v, _ := b.WebRecoveryBinding("one")
	for _, code := range []string{"UPSTREAM_RATE_LIMITED", "UPSTREAM_FAILURE", "ALIAS_CONFIRMATION_REQUIRED", "APPLE_ACCOUNT_ACTION_REQUIRED"} {
		s.noteWebRecovery(v, &BackendError{Code: code})
	}
	s.noteWebRecovery(account.RecoveryBinding{}, &BackendError{Code: "UPSTREAM_UNAUTHORIZED"})
	if len(s.recovery.records) != 0 {
		t.Fatal("unrelated error enqueued login")
	}
}
