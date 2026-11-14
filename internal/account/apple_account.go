package account

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"icloud-hme/internal/hme"
	"icloud-hme/internal/icloudmail/apple"
	"icloud-hme/internal/icloudmail/netproxy"
)

var ErrAppleOTPRequired = errors.New("Apple Account OTP required")
var ErrAppleOTPExpired = errors.New("Apple Account OTP session expired")
var ErrAppleSessionChanged = errors.New("Apple Account configuration changed")

type pendingAppleLogin struct {
	session                      apple.AccountSession
	expires                      time.Time
	identity, proxy, host        string
	challenge                    string
	importSourceID, verifiedDSID string
	importRevision               uint64
	mailbox                      string
}

func (m *Manager) appleLock(id string) *sync.Mutex {
	v, _ := m.appleLocks.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func appleContext(ctx context.Context, a *Account) context.Context {
	if a.Proxy == "" {
		return ctx
	}
	return apple.WithProxy(ctx, netproxy.Config{Endpoint: a.Proxy})
}

// Called under the per-account Apple lock. Never writes secrets into public DTOs.
func (m *Manager) saveAppleSession(snapshot *Account, session apple.AccountSession) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.accounts[snapshot.ID]
	if a == nil {
		return fmt.Errorf("账号不存在")
	}
	if a.appleLoginIdentity() != snapshot.appleLoginIdentity() || a.ICloudEmail != snapshot.ICloudEmail ||
		a.ImportSourceID != snapshot.ImportSourceID || a.VerifiedDSID != snapshot.VerifiedDSID ||
		a.ImportRevision != snapshot.ImportRevision || a.Proxy != snapshot.Proxy || a.Host != snapshot.Host {
		return ErrAppleSessionChanged
	}
	old := a.AppleSession
	a.AppleSession = &session
	if err := m.save(); err != nil {
		a.AppleSession = old
		return err
	}
	return nil
}

func (m *Manager) LoginAppleAccount(ctx context.Context, id, password, code string) (Summary, error) {
	lock := m.appleLock(id)
	lock.Lock()
	defer lock.Unlock()
	return m.loginAppleAccountLocked(ctx, id, password, code, "", false)
}

func (m *Manager) loginAppleAccountLocked(ctx context.Context, id, password, code, challenge string, deviceCode bool) (Summary, error) {
	a, ok := m.GetAccount(id)
	if !ok {
		return Summary{}, fmt.Errorf("账号不存在")
	}
	ctx = appleContext(ctx, a)
	var s apple.AccountSession
	var err error
	if code != "" {
		v, found := m.applePending.Load(id)
		if !found {
			return Summary{}, ErrAppleOTPExpired
		}
		p := v.(pendingAppleLogin)
		if p.challenge != challenge {
			return Summary{}, ErrAppleOTPExpired
		}
		if time.Now().After(p.expires) || !p.matchesAccount(a) {
			m.applePending.Delete(id)
			return Summary{}, ErrAppleOTPExpired
		}
		// Verification source is explicit: a device code must never go to the
		// phone/securitycode endpoint merely because Apple selected a phone.
		attempt := p.session
		if deviceCode {
			verification := apple.WebVerification{}
			if p.session.Verification != nil {
				verification = *p.session.Verification
			}
			verification.Method, verification.PhoneID = "device", 0
			attempt.Verification = &verification
		}
		s, err = m.appleClient.VerifyAccountCode(ctx, attempt, code)
		if err != nil {
			// Keep rotated headers/cookies but retain the original SMS option
			// for a later, explicitly selected phone-code attempt.
			if deviceCode {
				s.Verification = p.session.Verification
			}
			p.session = s
			m.applePending.Store(id, p)
			return Summary{}, err
		}
	} else {
		m.applePending.Delete(id)
		region := apple.RegionGlobal
		if a.Host == "icloud.com.cn" {
			region = apple.RegionChina
		}
		var pending bool
		s, pending, err = m.appleClient.SignInAccount(ctx, a.appleLoginIdentity(), password, region, a.AppleSession)
		if err != nil {
			return Summary{}, err
		}
		if pending {
			m.applePending.Store(id, pendingAppleLogin{session: s, expires: time.Now().Add(pendingLoginTTL), identity: a.appleLoginIdentity(), proxy: a.Proxy, host: a.Host, challenge: challenge, importSourceID: a.ImportSourceID, verifiedDSID: a.VerifiedDSID, importRevision: a.ImportRevision, mailbox: a.ICloudEmail})
			return Summary{}, ErrAppleOTPRequired
		}
	}
	m.applePending.Delete(id)
	if err = m.saveAppleSession(a, s); err != nil {
		return Summary{}, err
	}
	a, ok = m.GetAccount(id)
	if !ok {
		return Summary{}, fmt.Errorf("账号不存在")
	}
	return a.Summary(), nil
}

// New channel creation keeps its management cookies separate from Web cookies.
// Ambiguous completion is surfaced with the candidate, never regenerated here.
func (m *Manager) CreateAppleAlias(ctx context.Context, id, label string) (*hme.CreateResult, error) {
	lock := m.appleLock(id)
	lock.Lock()
	defer lock.Unlock()
	a, ok := m.GetAccount(id)
	if !ok {
		return nil, fmt.Errorf("账号不存在")
	}
	if a.AppleSession == nil {
		return nil, apple.ErrInvalidSession
	}
	alias, session, err := m.appleClient.CreateAccountAlias(appleContext(ctx, a), *a.AppleSession, label, "Created by icloud-hme")
	if err == nil {
		confirmed, updated, detailErr := m.appleClient.GetAccountAlias(appleContext(ctx, a), session, alias)
		session = updated
		if detailErr != nil {
			err = fmt.Errorf("%w: creation confirmation", apple.ErrInvalidResponse)
		} else {
			alias = confirmed
		}
	}
	if errors.Is(err, apple.ErrInvalidSession) {
		session.RefreshRejected = true
	}
	saveErr := m.saveAppleSession(a, session)
	result := &hme.CreateResult{Email: alias.HME, Label: label, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	if err != nil {
		return result, err
	}
	if saveErr != nil {
		return result, saveErr
	}
	if !alias.IsActive {
		return result, apple.ErrInvalidResponse
	}
	return result, nil
}

func (m *Manager) startAppleMaintenance() error {
	client, err := apple.NewClient(apple.Config{})
	if err != nil {
		return err
	}
	m.appleClient = client
	ctx, cancel := context.WithCancel(context.Background())
	m.appleCancel = cancel
	m.appleDone = make(chan struct{})
	go func() {
		defer close(m.appleDone)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.applePending.Range(func(k, v any) bool {
					lock := m.appleLock(k.(string))
					if lock.TryLock() {
						if current, ok := m.applePending.Load(k); ok && time.Now().After(current.(pendingAppleLogin).expires) {
							m.applePending.Delete(k)
						}
						lock.Unlock()
					}
					return true
				})
				for _, a := range m.ListSummaries() {
					if ctx.Err() != nil {
						return
					}
					m.refreshAppleAccount(ctx, a.ID)
				}
			}
		}
	}()
	return nil
}

func (m *Manager) refreshAppleAccount(ctx context.Context, id string) {
	lock := m.appleLock(id)
	if !lock.TryLock() {
		return
	}
	defer lock.Unlock()
	a, ok := m.GetAccount(id)
	if !ok || a.AppleSession == nil || !a.AppleSession.NeedsRefresh(time.Now()) {
		return
	}
	callCtx, cancel := context.WithTimeout(appleContext(ctx, a), 60*time.Second)
	defer cancel()
	s, err := m.appleClient.RefreshAccountSession(callCtx, *a.AppleSession)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		s.RefreshFailures++
		s.RefreshRejected = errors.Is(err, apple.ErrInvalidSession) || errors.Is(err, apple.ErrAuthentication)
		delay := time.Minute * time.Duration(1<<min(s.RefreshFailures, 6))
		s.RefreshAfter = time.Now().Add(max(delay, apple.RetryDelay(err)))
	}
	_ = m.saveAppleSession(a, s)
}

// Imported Apple IDs may be external email addresses or phone numbers; their
// iCloud mailbox is not the credential identity supplied by the Windows client.
// Keep the historical login choice for accounts not imported through validation.
func (a *Account) appleLoginIdentity() string {
	if a.ImportSourceID != "" && a.VerifiedDSID != "" &&
		a.ImportSourceID == ImportSourceID(a.VerifiedAppleID) &&
		normalizeIdentity(a.VerifiedAppleID) == normalizeIdentity(a.RealEmail) {
		return normalizeIdentity(a.VerifiedAppleID)
	}
	return a.loginEmail()
}
func (p pendingAppleLogin) matchesAccount(a *Account) bool {
	return p.identity == a.appleLoginIdentity() && p.proxy == a.Proxy && p.host == a.Host &&
		p.importSourceID == a.ImportSourceID && p.verifiedDSID == a.VerifiedDSID &&
		p.importRevision == a.ImportRevision && p.mailbox == a.ICloudEmail
}
