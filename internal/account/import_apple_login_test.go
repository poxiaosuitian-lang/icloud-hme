package account

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"icloud-hme/internal/icloudmail/apple"
)

type importLoginTransport func(*http.Request) (*http.Response, error)

func (f importLoginTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestImportAppleLoginBindingAndChallenge(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	// Stop maintenance before installing a fixture transport.
	m.appleCancel()
	<-m.appleDone
	source := ImportSourceID("owner@example.test")
	nonce := strings.Repeat("a", 64)
	m.accounts["fixture"] = &Account{ID: "fixture", RealEmail: "owner@example.test", ImportSourceID: source, Host: "icloud.com"}
	calls := 0
	reject := false
	m.appleClient, err = apple.NewClient(apple.Config{Transport: importLoginTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		body := "{}"
		status := 200
		switch r.URL.Path {
		case "/appleauth/auth/verify/trusteddevice/securitycode":
			if reject {
				status = 400
			}
		case "/appleauth/auth/2sv/trust":
			status = 204
		case "/account/manage/gs/ws/token":
			body = `{"timeOutInterval":1800}`
		case "/account/manage":
			body = `{"apiKey":"fixture-private-api-key"}`
		case "/account/manage/forwardemail":
		default:
			t.Errorf("unexpected mutation/path %s", r.URL.Path)
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	req := ImportAppleLoginRequest{Version: 1, SourceID: source, AppleID: "owner@example.test", Operation: "status"}
	out, err := m.ImportAppleLogin(context.Background(), req)
	if err != nil || out.State != "login_required" || calls != 0 {
		t.Fatal("missing session status")
	}
	bad := req
	bad.AppleID = "other@example.test"
	bad.SourceID = ImportSourceID(bad.AppleID)
	if _, err = m.ImportAppleLogin(context.Background(), bad); err != ImportConflict || calls != 0 {
		t.Fatal("wrong identity reached Apple")
	}
	pending := pendingAppleLogin{session: apple.AccountSession{AppleID: req.AppleID, SCNT: "fixture-scnt", FrameID: "fixture-frame"}, expires: time.Now().Add(time.Minute), identity: req.AppleID, host: "icloud.com", challenge: nonce}
	m.applePending.Store("fixture", pending)
	req.Operation = "verify"
	req.Challenge = strings.Repeat("b", 64)
	req.Code = "123456"
	if _, err = m.ImportAppleLogin(context.Background(), req); !errors.Is(err, ErrAppleOTPExpired) || calls != 0 {
		t.Fatal("wrong challenge reached Apple")
	}
	if _, exists := m.applePending.Load("fixture"); !exists {
		t.Fatal("another challenge was destroyed")
	}
	if _, err = m.LoginAppleAccount(context.Background(), "fixture", "", "123456"); !errors.Is(err, ErrAppleOTPExpired) || calls != 0 {
		t.Fatal("browser consumed integration challenge")
	}
	req.Operation = "login"
	req.Password = "fixture-secret-password"
	req.Code = ""
	req.Challenge = ""
	out, err = m.ImportAppleLogin(context.Background(), req)
	if err != nil || out.Challenge != nonce || calls != 0 {
		t.Fatal("pending login was repeated")
	}
	req.Operation = "verify"
	req.Password = ""
	req.Code = "123456"
	req.Challenge = nonce
	reject = true
	out, err = m.ImportAppleLogin(context.Background(), req)
	if err != nil || out.State != "otp_required" || calls != 1 {
		t.Fatal("invalid code did not preserve challenge")
	}
	reject = false
	out, err = m.ImportAppleLogin(context.Background(), req)
	if err != nil || out.State != "ready" {
		t.Fatal("verification failed", err)
	}
	if _, exists := m.applePending.Load("fixture"); exists {
		t.Fatal("accepted challenge not consumed")
	}
	before := calls
	if _, err = m.ImportAppleLogin(context.Background(), req); !errors.Is(err, ErrAppleOTPExpired) || calls != before {
		t.Fatal("challenge replay accepted")
	}
	req.Operation = "status"
	req.Code = ""
	req.Challenge = ""
	out, err = m.ImportAppleLogin(context.Background(), req)
	if err != nil || out.State != "ready" || calls != before+3 {
		t.Fatal("status did not verify actual management session")
	}
	saved, _ := m.GetAccount("fixture")
	if saved.AppleSession == nil || saved.AppleSession.APIKey != "fixture-private-api-key" {
		t.Fatal("session was not saved")
	}
}
