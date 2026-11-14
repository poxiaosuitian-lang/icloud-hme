package account

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"icloud-hme/internal/icloudmail/apple"
)

func TestRecoveryVerifiedBindingAndReadonlyReady(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.appleCancel()
	<-m.appleDone
	a := &Account{ID: "fixture", RealEmail: "owner@example.test", VerifiedAppleID: "owner@example.test", VerifiedDSID: "12345", ICloudEmail: "receive@icloud.com", ImportSourceID: ImportSourceID("owner@example.test"), ImportRevision: 1, Host: "icloud.com", AppPassword: "fixture-app-secret"}
	m.accounts[a.ID] = a
	expected, ok := m.RecoveryBinding(a.ID)
	if !ok {
		t.Fatal("verified binding rejected")
	}
	calls := 0
	m.appleClient, err = apple.NewClient(apple.Config{Transport: importLoginTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "GET" {
			t.Error("recovery performed mutation", r.Method, r.URL.Path)
		}
		body := "{}"
		switch r.URL.Path {
		case "/account/manage/gs/ws/token":
			body = `{"timeOutInterval":1800}`
		case "/account/manage":
			body = `{"apiKey":"fixture-key"}`
		case "/account/manage/forwardemail":
		default:
			t.Error("unexpected path", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	if err = m.VerifyRecovery(context.Background(), expected); err == nil || calls != 0 {
		t.Fatal("missing session treated as ready")
	}
	a.AppleSession = &apple.AccountSession{AppleID: a.VerifiedAppleID, SCNT: "fixture-scnt", APIKey: "fixture-key", AuthenticatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	current, _ := m.RecoveryBinding(a.ID)
	if err = m.VerifyRecovery(context.Background(), expected); err == nil || calls != 0 {
		t.Fatal("stale version reached Apple")
	}
	if err = m.VerifyRecovery(context.Background(), current); err != nil || calls < 3 {
		t.Fatal("no real readonly verification", err, calls)
	}
	// Local binding reads cannot refresh sessions, regardless of TTL.
	calls = 0
	for i := 0; i < 10; i++ {
		_, _ = m.RecoveryBinding(a.ID)
	}
	if calls != 0 {
		t.Fatal("local metadata triggered network")
	}
	a.VerifiedDSID = ""
	if _, ok = m.RecoveryBinding(a.ID); ok {
		t.Fatal("forged source alone accepted")
	}
}
func TestRecoveryBindingFencesConfigChangesAndReverts(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	a := &Account{ID: "fixture", RealEmail: "owner@example.test", VerifiedAppleID: "owner@example.test", VerifiedDSID: "123", ICloudEmail: "receive@icloud.com", ImportSourceID: ImportSourceID("owner@example.test"), ImportRevision: 1, Host: "icloud.com"}
	m.accounts[a.ID] = a
	before, _ := m.RecoveryBinding(a.ID)
	if _, err = m.UpdateProxy(a.ID, "http://127.0.0.1:1234"); err != nil {
		t.Fatal(err)
	}
	if _, err = m.UpdateProxy(a.ID, ""); err != nil {
		t.Fatal(err)
	}
	after, _ := m.RecoveryBinding(a.ID)
	if after.Binding == before.Binding {
		t.Fatal("config revert resurrected old lease")
	}
	a.RealEmail = "wrong@example.test"
	if _, ok := m.RecoveryBinding(a.ID); ok {
		t.Fatal("identity mismatch accepted")
	}
}
