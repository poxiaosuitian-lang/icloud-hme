package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/mail"
	"strings"
	"time"
)

// RecoveryBinding contains opaque versions, never credentials. It is a local
// snapshot, not a network session check. Import revision fences re-syncs.
type RecoveryBinding struct {
	AccountID string `json:"account_id"`
	SourceID  string `json:"source_id"`
	Binding   string `json:"binding"`
	Session   string `json:"session"`
}

func AppleSessionRevision(a *Account) string {
	if a == nil || a.AppleSession == nil {
		return "false"
	}
	return a.AppleSession.AuthenticatedAt.UTC().Format(time.RFC3339Nano)
}
func (m *Manager) RecoveryBinding(id string) (RecoveryBinding, bool) {
	a, ok := m.GetAccount(id)
	if !ok {
		return RecoveryBinding{}, false
	}
	return recoveryBindingFor(a)
}
func recoveryBindingFor(a *Account) (RecoveryBinding, bool) {
	if a == nil || normalizeIdentity(a.VerifiedAppleID) == "" || a.ImportRevision == 0 || a.VerifiedDSID == "" || a.ImportSourceID != ImportSourceID(a.VerifiedAppleID) || normalizeIdentity(a.RealEmail) != normalizeIdentity(a.VerifiedAppleID) || (a.Host != "icloud.com" && a.Host != "icloud.com.cn") {
		return RecoveryBinding{}, false
	}
	address, err := mail.ParseAddress(a.ICloudEmail)
	if err != nil || address.Address != a.ICloudEmail || !strings.Contains(a.ICloudEmail, "@") {
		return RecoveryBinding{}, false
	}
	// Never store password/proxy contents in the queue. Hash all configuration
	// affecting identity and authentication, including manual app-password edits.
	raw, _ := json.Marshal([]any{a.ID, a.ImportSourceID, normalizeIdentity(a.VerifiedAppleID), a.VerifiedDSID, a.ImportRevision, a.RecoveryRevision, a.RealEmail, a.ICloudEmail, a.Host, a.Proxy, a.AppPassword})
	digest := sha256.Sum256(raw)
	clear(raw)
	return RecoveryBinding{a.ID, a.ImportSourceID, hex.EncodeToString(digest[:]), AppleSessionRevision(a)}, true
}
func (m *Manager) VerifyRecovery(ctx context.Context, expected RecoveryBinding) error {
	before, ok := m.RecoveryBinding(expected.AccountID)
	if !ok || before != expected {
		return ImportConflict
	}
	a, ok := m.GetAccount(expected.AccountID)
	if !ok || a.AppleSession == nil || normalizeIdentity(a.AppleSession.AppleID) != normalizeIdentity(a.VerifiedAppleID) {
		return ImportConflict
	}
	out, err := m.ImportAppleLogin(ctx, ImportAppleLoginRequest{Version: 1, SourceID: expected.SourceID, AppleID: a.VerifiedAppleID, Operation: "status"})
	if err != nil {
		return err
	}
	after, ok := m.RecoveryBinding(expected.AccountID)
	if !ok || after != expected || out.State != "ready" {
		return ImportConflict
	}
	return nil
}
