package account

import (
	"context"
	"strconv"
	"time"
)

type WebRecoveryRequest struct {
	Version  int               `json:"version"`
	ClientID string            `json:"client_id"`
	JobID    string            `json:"job_id"`
	Lease    string            `json:"lease"`
	SourceID string            `json:"source_id"`
	AppleID  string            `json:"apple_id"`
	Host     string            `json:"host"`
	Cookies  map[string]string `json:"cookies"`
}

// Validated material stays in memory; neither a new account nor other account
// settings can be supplied by the recovery caller.
type WebRecoveryUpdate struct {
	expected RecoveryBinding
	version  [32]byte
	cookies  map[string]string
}

func (m *Manager) WebRecoveryBinding(id string) (RecoveryBinding, bool) {
	a, ok := m.GetAccount(id)
	if !ok {
		return RecoveryBinding{}, false
	}
	return webRecoveryBindingFor(a)
}
func webRecoveryBindingFor(a *Account) (RecoveryBinding, bool) {
	binding, ok := recoveryBindingFor(a)
	if !ok {
		return RecoveryBinding{}, false
	}
	binding.Session = "web:" + strconv.FormatUint(a.WebRecoveryRevision, 10)
	return binding, true
}

func (m *Manager) PrepareWebRecovery(ctx context.Context, expected RecoveryBinding, r WebRecoveryRequest) (*WebRecoveryUpdate, error) {
	if importAppleClientError != nil {
		return nil, ImportUnavailable
	}
	return m.prepareWebRecovery(ctx, expected, r, realImportVerifier{importAppleClient})
}
func (m *Manager) prepareWebRecovery(ctx context.Context, expected RecoveryBinding, r WebRecoveryRequest, v importVerifier) (*WebRecoveryUpdate, error) {
	a, ok := m.GetAccount(expected.AccountID)
	if !ok || r.SourceID != expected.SourceID || normalizeIdentity(r.AppleID) != normalizeIdentity(a.VerifiedAppleID) || r.Host != a.Host {
		return nil, ImportConflict
	}
	current, valid := webRecoveryBindingFor(a)
	if !valid || current != expected {
		return nil, ImportConflict
	}
	check := ImportRequest{Version: r.Version, SourceID: r.SourceID, AppleID: r.AppleID, Host: r.Host, Cookies: r.Cookies, ICloudEmail: a.ICloudEmail, AppPassword: a.AppPassword}
	if check.NormalizeAndValidate() != nil {
		return nil, ImportInvalid
	}
	version := accountsVersion(map[string]*Account{a.ID: a})
	session, err := v.identity(ctx, r.Host, r.Cookies)
	if err != nil {
		return nil, importUpstreamError(err, ImportAppleFailed)
	}
	if session.HSAChallengeRequired || session.DSID != a.VerifiedDSID || normalizeIdentity(session.ValidatedAppleID) != normalizeIdentity(a.VerifiedAppleID) {
		return nil, ImportConflict
	}
	list, updated, err := v.directory(ctx, session)
	if err != nil {
		return nil, importUpstreamError(err, ImportHMEFailed)
	}
	if !verifiedImportMailbox(a.ICloudEmail, list) {
		return nil, ImportConflict
	}
	cookies := cloneCookies(r.Cookies)
	for _, c := range updated.Cookies {
		if c.Domain == r.Host || c.Domain == "."+r.Host {
			if c.Path == "/" && !c.HostOnly && (c.Expires.IsZero() || c.Expires.After(time.Now())) {
				cookies[c.Name] = c.Value
			}
		}
	}
	return &WebRecoveryUpdate{expected, version, cookies}, nil
}

// The server holds the lease mutex around this short CAS. All Apple requests
// finished in PrepareWebRecovery, so no network calls happen under either lock.
func (m *Manager) CommitWebRecovery(update *WebRecoveryUpdate) error {
	if update == nil {
		return ImportInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.accounts[update.expected.AccountID]
	if current == nil || accountsVersion(map[string]*Account{current.ID: current}) != update.version {
		return ImportConflict
	}
	next := copyAccount(current)
	next.Cookies = cloneCookies(update.cookies)
	next.WebRecoveryRevision++
	next.Status = "active"
	next.LastError = ""
	next.LastValidated = time.Now().UTC().Format(time.RFC3339Nano)
	m.accounts[next.ID] = next
	if m.save() != nil {
		m.accounts[next.ID] = current
		return ImportStorageFailed
	}
	return nil
}

func (m *Manager) VerifyWebRecovery(ctx context.Context, expected RecoveryBinding) error {
	a, ok := m.GetAccount(expected.AccountID)
	if !ok {
		return ImportConflict
	}
	_, err := m.PrepareWebRecovery(ctx, expected, WebRecoveryRequest{Version: 1, SourceID: expected.SourceID, AppleID: a.VerifiedAppleID, Host: a.Host, Cookies: a.Cookies})
	if err != nil {
		return err
	}
	after, ok := m.WebRecoveryBinding(expected.AccountID)
	if !ok || after != expected {
		return ImportConflict
	}
	return nil
}
