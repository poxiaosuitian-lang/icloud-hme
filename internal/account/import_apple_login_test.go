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
	m.accounts["fixture"] = &Account{ID: "fixture", RealEmail: "owner@example.test", VerifiedAppleID: "owner@example.test", VerifiedDSID: "12345", ICloudEmail: "different@icloud.com", ImportSourceID: source, Host: "icloud.com"}
	calls := 0
	reject := false
	m.appleClient, err = apple.NewClient(apple.Config{Transport: importLoginTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		body := "{}"
		status := 200
		headers := make(http.Header)
		switch r.URL.Path {
		case "/appleauth/auth/verify/trusteddevice/securitycode":
			if reject {
				headers.Set("scnt", "fixture-rotated-scnt")
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
		return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
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
	pending := pendingAppleLogin{session: apple.AccountSession{AppleID: req.AppleID, SCNT: "fixture-scnt", FrameID: "fixture-frame", Verification: &apple.WebVerification{Method: "sms", PhoneID: 7, Phones: []apple.TrustedPhone{{ID: 7}}}}, expires: time.Now().Add(time.Minute), identity: req.AppleID, host: "icloud.com", challenge: nonce, importSourceID: source, verifiedDSID: "12345", mailbox: "different@icloud.com"}
	m.applePending.Store("fixture", pending)
	req.Operation = "verify_device"
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
	req.Operation = "verify_device"
	req.Password = ""
	req.Code = "123456"
	req.Challenge = nonce
	reject = true
	out, err = m.ImportAppleLogin(context.Background(), req)
	if err != nil || out.State != "otp_required" || calls != 1 {
		t.Fatal("invalid code did not preserve challenge")
	}
	storedPending, _ := m.applePending.Load("fixture")
	if storedPending.(pendingAppleLogin).session.Verification.Method != "sms" || storedPending.(pendingAppleLogin).session.SCNT != "fixture-rotated-scnt" {
		t.Fatal("device failure discarded SMS option")
	}
	if !out.DeviceCodeSupported || out.Verification != "sms" {
		t.Fatal("verification capabilities missing")
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

func TestImportAppleLoginRequiresVerifiedBinding(t *testing.T) {
	for _, mode := range []string{"source_only", "wrong_verified_id", "missing_dsid", "changed_identity", "duplicate", "valid_external_id"} {
		t.Run(mode, func(t *testing.T) {
			m := importManager(t)
			m.appleCancel()
			<-m.appleDone
			id := "owner@example.test"
			a := &Account{ID: "fixture", RealEmail: id, VerifiedAppleID: id, VerifiedDSID: "12345", ICloudEmail: "different@icloud.com", ImportSourceID: ImportSourceID(id), Host: "icloud.com"}
			m.accounts[a.ID] = a
			switch mode {
			case "source_only":
				a.VerifiedAppleID = ""
				a.VerifiedDSID = ""
			case "wrong_verified_id":
				a.VerifiedAppleID = "other@example.test"
			case "missing_dsid":
				a.VerifiedDSID = ""
			case "changed_identity":
				a.RealEmail = "other@example.test"
			case "duplicate":
				duplicate := copyAccount(a)
				duplicate.ID = "duplicate"
				m.accounts[duplicate.ID] = duplicate
			}
			m.appleClient, _ = apple.NewClient(apple.Config{Transport: importLoginTransport(func(*http.Request) (*http.Response, error) {
				t.Error("unexpected Apple call")
				return nil, errors.New("unexpected")
			})})
			out, err := m.ImportAppleLogin(context.Background(), ImportAppleLoginRequest{Version: 1, SourceID: ImportSourceID(id), AppleID: id, Operation: "status"})
			if mode == "valid_external_id" {
				if err != nil || out.State != "login_required" {
					t.Fatal("valid imported identity rejected", err)
				}
			} else if err != ImportConflict {
				t.Fatal("unverified or conflicting binding accepted", err)
			}
		})
	}
}

func TestIntegrationChallengeInvalidatedByReimport(t *testing.T) {
	m := importManager(t)
	m.appleCancel()
	<-m.appleDone
	id := "owner@example.test"
	source := ImportSourceID(id)
	nonce := strings.Repeat("a", 64)
	a := &Account{ID: "fixture", RealEmail: id, VerifiedAppleID: id, VerifiedDSID: "12345", ICloudEmail: "different@icloud.com", ImportSourceID: source, Host: "icloud.com", ImportRevision: 2}
	m.accounts[a.ID] = a
	m.applePending.Store(a.ID, pendingAppleLogin{identity: id, host: a.Host, expires: time.Now().Add(time.Minute), challenge: nonce, importSourceID: source, verifiedDSID: a.VerifiedDSID, mailbox: a.ICloudEmail, importRevision: 1})
	m.appleClient, _ = apple.NewClient(apple.Config{Transport: importLoginTransport(func(*http.Request) (*http.Response, error) {
		t.Error("stale challenge reached Apple")
		return nil, errors.New("unexpected")
	})})
	_, err := m.ImportAppleLogin(context.Background(), ImportAppleLoginRequest{Version: 1, SourceID: source, AppleID: id, Operation: "verify", Challenge: nonce, Code: "123456"})
	if !errors.Is(err, ErrAppleOTPExpired) {
		t.Fatal("old import challenge accepted", err)
	}
	stale := copyAccount(a)
	stale.ImportRevision--
	if err = m.saveAppleSession(stale, apple.AccountSession{APIKey: "stale-key"}); !errors.Is(err, ErrAppleSessionChanged) {
		t.Fatal("stale login overwrote reimport", err)
	}
}
