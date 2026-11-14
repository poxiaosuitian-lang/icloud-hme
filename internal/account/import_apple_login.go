package account

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"time"

	"icloud-hme/internal/icloudmail/apple"
)

// Passwords and OTPs exist only for the duration of this request. No password is
// retained for renewal; status validation is explicitly requested, never periodic.
type ImportAppleLoginRequest struct {
	Version   int    `json:"version"`
	SourceID  string `json:"source_id"`
	AppleID   string `json:"apple_id"`
	Operation string `json:"operation"`
	Password  string `json:"password,omitempty"`
	Code      string `json:"otp_code,omitempty"`
	Challenge string `json:"challenge,omitempty"`
}
type ImportAppleLoginResult struct {
	DeviceCodeSupported bool   `json:"device_code_supported"`
	SourceID            string `json:"source_id"`
	State               string `json:"state"`
	Challenge           string `json:"challenge,omitempty"`
	Verification        string `json:"verification,omitempty"`
}

func (r *ImportAppleLoginRequest) Validate() error {
	r.AppleID = normalizeIdentity(r.AppleID)
	if r.Version != 1 || r.AppleID == "" || len(r.AppleID) > 254 || r.SourceID != ImportSourceID(r.AppleID) {
		return ImportInvalid
	}
	switch r.Operation {
	case "status":
		if r.Password != "" || r.Code != "" || r.Challenge != "" {
			return ImportInvalid
		}
	case "login":
		if r.Password == "" || len(r.Password) > 1024 || r.Code != "" || r.Challenge != "" {
			return ImportInvalid
		}
	case "verify", "verify_device":
		if r.Password != "" || !regexp.MustCompile(`^[0-9]{6}$`).MatchString(r.Code) || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(r.Challenge) {
			return ImportInvalid
		}
	default:
		return ImportInvalid
	}
	return nil
}
func (m *Manager) ImportAppleLogin(ctx context.Context, r ImportAppleLoginRequest) (ImportAppleLoginResult, error) {
	out := ImportAppleLoginResult{SourceID: r.SourceID, DeviceCodeSupported: true}
	if err := r.Validate(); err != nil {
		return out, err
	}
	// Only a previously verified import can resolve the target. No caller-supplied
	// server account ID, URL, proxy or cookie participates in authentication.
	m.mu.RLock()
	id := ""
	for _, a := range m.accounts {
		if a != nil && a.ImportSourceID == r.SourceID {
			if !matchesImportAppleIdentity(a, r) {
				m.mu.RUnlock()
				return out, ImportConflict
			}
			if id != "" {
				m.mu.RUnlock()
				return out, ImportConflict
			}
			id = a.ID
		}
	}
	m.mu.RUnlock()
	if id == "" {
		return out, ImportConflict
	}
	lock := m.appleLock(id)
	if !lock.TryLock() {
		return out, ImportUnavailable
	}
	defer lock.Unlock()
	a, ok := m.GetAccount(id)
	if !ok || !matchesImportAppleIdentity(a, r) {
		return out, ImportConflict
	}
	if r.Operation == "status" {
		if a.AppleSession != nil {
			session, err := m.appleClient.RefreshAccountSession(appleContext(ctx, a), *a.AppleSession)
			if err == nil {
				if err = m.saveAppleSession(a, session); err != nil {
					return out, ImportStorageFailed
				}
				out.State = "ready"
				return out, nil
			}
			if !errors.Is(err, apple.ErrInvalidSession) && !errors.Is(err, apple.ErrAuthentication) {
				return out, err
			}
		}
		out.State = "login_required"
		return out, nil
	}
	challenge := r.Challenge
	if r.Operation == "login" {
		// Reuse the exact unexpired pending challenge after a lost response. Never
		// send repeated login challenges while the same login is awaiting its OTP.
		if v, found := m.applePending.Load(id); found {
			p := v.(pendingAppleLogin)
			if p.challenge != "" && time.Now().Before(p.expires) && p.matchesAccount(a) {
				out.State = "otp_required"
				out.Challenge = p.challenge
				out.Verification = importVerificationMethod(p.session)
				return out, nil
			}
		}
		nonce := make([]byte, 32)
		if _, err := rand.Read(nonce); err != nil {
			return out, ImportUnavailable
		}
		challenge = hex.EncodeToString(nonce)
	}
	_, err := m.loginAppleAccountLocked(ctx, id, r.Password, r.Code, challenge, r.Operation == "verify_device")
	if errors.Is(err, ErrAppleOTPRequired) || errors.Is(err, apple.ErrTwoFactorCode) {
		out.State = "otp_required"
		out.Challenge = challenge
		if v, found := m.applePending.Load(id); found {
			out.Verification = importVerificationMethod(v.(pendingAppleLogin).session)
		}
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.State = "ready"
	return out, nil
}

func importVerificationMethod(s apple.AccountSession) string {
	if s.Verification != nil && s.Verification.Method == "sms" {
		return "sms"
	}
	return "trusted_device"
}

// Bind to the identity proven by Apple during import, not to the separate
// iCloud receiving address. Source metadata alone is not verification proof.
func matchesImportAppleIdentity(a *Account, r ImportAppleLoginRequest) bool {
	return a != nil && a.ImportSourceID == r.SourceID && a.VerifiedDSID != "" &&
		normalizeIdentity(a.VerifiedAppleID) == r.AppleID &&
		normalizeIdentity(a.RealEmail) == r.AppleID && a.ICloudEmail != ""
}
