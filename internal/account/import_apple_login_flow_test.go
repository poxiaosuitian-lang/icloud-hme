package account

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"icloud-hme/internal/icloudmail/apple"
)

func TestImportAppleLoginSRPToPersistedSession(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.appleCancel()
	<-m.appleDone
	identity := "owner@example.test"
	source := ImportSourceID(identity)
	m.accounts["fixture"] = &Account{ID: "fixture", RealEmail: identity, ImportSourceID: source, Host: "icloud.com"}
	completes, verifies := 0, 0
	m.appleClient, err = apple.NewClient(apple.Config{Transport: importLoginTransport(func(r *http.Request) (*http.Response, error) {
		status := 200
		body := "{}"
		headers := make(http.Header)
		headers.Set("scnt", "fixture-scnt")
		switch r.URL.Path {
		case "/account/manage/section/privacy":
			body = "<html/>"
		case "/bootstrap/portal":
			body = `{"timeOutInterval":1800}`
		case "/account/manage/gs/ws/token":
		case "/appleauth/auth/authorize/signin":
			headers.Set("X-Apple-HC-Bits", "1")
			headers.Set("X-Apple-HC-Challenge", "fixture")
		case "/appleauth/auth/verify/device/key/challenge", "/appleauth/auth/federate":
		case "/appleauth/auth/signin/init":
			body = `{"salt":"c2FsdFNhbHQ=","b":"Ag==","c":"Yw==","iteration":1000,"protocol":"s2k_fo"}`
		case "/appleauth/auth/signin/complete":
			completes++
			status = 409
		case "/appleauth/auth":
		case "/appleauth/auth/verify/trusteddevice/securitycode":
			verifies++
			status = 204
		case "/appleauth/auth/2sv/trust":
			status = 204
		case "/account/manage":
			body = `{"apiKey":"fixture-api-key"}`
		case "/account/manage/forwardemail":
		default:
			t.Errorf("unexpected Apple operation %s", r.URL.Path)
		}
		return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	req := ImportAppleLoginRequest{Version: 1, SourceID: source, AppleID: identity, Operation: "login", Password: "fixture-password-never-save"}
	first, err := m.ImportAppleLogin(context.Background(), req)
	if err != nil || first.State != "otp_required" || len(first.Challenge) != 64 || completes != 1 || first.Verification != "trusted_device" {
		t.Fatal("login challenge failed", err)
	}
	again, err := m.ImportAppleLogin(context.Background(), req)
	if err != nil || again.Challenge != first.Challenge || completes != 1 {
		t.Fatal("login challenge was not reused")
	}
	req.Operation = "verify"
	req.Password = ""
	req.Code = "123456"
	req.Challenge = first.Challenge
	verified, err := m.ImportAppleLogin(context.Background(), req)
	if err != nil || verified.State != "ready" || verifies != 1 {
		t.Fatal("verification failed", err)
	}
	raw, err := os.ReadFile(m.dataFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "fixture-password-never-save") || strings.Contains(string(raw), first.Challenge) || strings.Contains(string(raw), "123456") {
		t.Fatal("transient credentials persisted")
	}
	public, _ := json.Marshal(verified)
	if strings.Contains(string(public), "fixture-api-key") {
		t.Fatal("session leaked")
	}
	restored, err := NewManager(m.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	a, _ := restored.GetAccount("fixture")
	if a.AppleSession == nil || a.AppleSession.APIKey != "fixture-api-key" {
		t.Fatal("session not restored")
	}
}
