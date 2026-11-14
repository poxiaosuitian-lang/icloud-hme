package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"icloud-hme/internal/icloudmail/apple"
	"icloud-hme/internal/mail"
)

// ImportRequest is the versioned AppleServices wire protocol, not an Account.
// Deliberately no proxy, endpoint, mailbox configuration or second-channel fields.
type ImportRequest struct {
	Version     int               `json:"version"`
	SourceID    string            `json:"source_id"`
	AppleID     string            `json:"apple_id"`
	Name        string            `json:"name"`
	ICloudEmail string            `json:"icloud_email"`
	Host        string            `json:"host"`
	Cookies     map[string]string `json:"cookies"`
	AppPassword string            `json:"app_password"`
}

type ImportResult struct {
	ID           string `json:"id"`
	SourceID     string `json:"source_id"`
	Status       string `json:"status"`
	HMEVerified  bool   `json:"hme_verified"`
	IMAPVerified bool   `json:"imap_verified"`
}

// ImportError deliberately contains no upstream error, request data or secrets.
type ImportError string

func (e ImportError) Error() string { return string(e) }

const (
	ImportInvalid       ImportError = "INVALID_IMPORT_REQUEST"
	ImportConflict      ImportError = "ACCOUNT_IDENTITY_CONFLICT"
	ImportAppleFailed   ImportError = "APPLE_VERIFICATION_FAILED"
	ImportHMEFailed     ImportError = "HME_VERIFICATION_FAILED"
	ImportIMAPFailed    ImportError = "IMAP_VERIFICATION_FAILED"
	ImportUnavailable   ImportError = "IMPORT_UNAVAILABLE"
	ImportStorageFailed ImportError = "IMPORT_STORAGE_FAILED"
)

func normalizeIdentity(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
func ImportSourceID(appleID string) string {
	sum := sha256.Sum256([]byte("appleservices-hme-v1\x00" + normalizeIdentity(appleID)))
	return "appleservices-" + hex.EncodeToString(sum[:])
}

var importIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var appPasswordPattern = regexp.MustCompile(`^[a-zA-Z]{4}(?:-[a-zA-Z]{4}){3}$|^[a-zA-Z]{16}$`)

func (r *ImportRequest) NormalizeAndValidate() error {
	r.AppleID = normalizeIdentity(r.AppleID)
	r.ICloudEmail = normalizeIdentity(r.ICloudEmail)
	r.Name = strings.TrimSpace(r.Name)
	if r.Version != 1 || r.SourceID != ImportSourceID(r.AppleID) ||
		(r.Host != "icloud.com" && r.Host != "icloud.com.cn") ||
		len(r.AppleID) > 254 || validateEmail(r.AppleID) != nil ||
		len(r.ICloudEmail) > 254 || validateEmail(r.ICloudEmail) != nil || !isImportMailboxDomain(r.ICloudEmail) ||
		len([]rune(r.Name)) > 64 || strings.IndexFunc(r.Name, unicode.IsControl) >= 0 ||
		!appPasswordPattern.MatchString(r.AppPassword) || len(r.Cookies) == 0 || len(r.Cookies) > 128 {
		return ImportInvalid
	}
	// Cookies are HTTP header values; reject rather than silently dropping bytes.
	cookies := make(map[string]string, len(r.Cookies))
	for name, value := range r.Cookies {
		value = strings.Trim(value, `"`)
		if len(name) > 256 || len(value) > 16384 || (&http.Cookie{Name: name, Value: value}).Valid() != nil {
			return ImportInvalid
		}
		cookies[name] = value
	}
	if cookies["X-APPLE-WEBAUTH-TOKEN"] == "" || cookies["X-APPLE-WEBAUTH-USER"] == "" {
		return ImportInvalid
	}
	r.Cookies = cookies
	return nil
}

type importVerifier interface {
	identity(context.Context, string, map[string]string) (apple.Session, error)
	directory(context.Context, apple.Session) (apple.ListResult, apple.Session, error)
	imap(context.Context, string, string) error
}
type realImportVerifier struct{ client *apple.Client }

func (v realImportVerifier) identity(ctx context.Context, host string, cookies map[string]string) (apple.Session, error) {
	s := apple.Session{Region: apple.RegionGlobal}
	if host == "icloud.com.cn" {
		s.Region = apple.RegionChina
	}
	for k, value := range cookies {
		s.Cookies = append(s.Cookies, apple.PersistentCookie{Name: k, Value: strings.Trim(value, `"`), Domain: host, Path: "/", Secure: true})
	}
	return v.client.Validate(ctx, s)
}
func (v realImportVerifier) directory(ctx context.Context, s apple.Session) (apple.ListResult, apple.Session, error) {
	return v.client.ListAliases(ctx, s)
}
func (v realImportVerifier) imap(ctx context.Context, email, password string) error {
	return mail.VerifyICloudCredentials(ctx, email, password)
}
func importUpstreamError(err error, fallback ImportError) error {
	var upstream *apple.Error
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, mail.ErrIMAPUnavailable) ||
		apple.IsRateLimited(err) || (errors.As(err, &upstream) && (upstream.Retryable || upstream.StatusCode >= 500)) {
		return ImportUnavailable
	}
	return fallback
}

var importAppleClient, importAppleClientError = apple.NewClient(apple.Config{Timeout: 25 * time.Second})

// ImportAppleServices serializes imports, not the manager: network verification
// runs without mu. A content-version snapshot also catches concurrent legacy
// mutations (including second-channel refreshes) before the single commit.
func (m *Manager) ImportAppleServices(ctx context.Context, r ImportRequest) (ImportResult, bool, error) {
	if importAppleClientError != nil {
		return ImportResult{}, false, ImportUnavailable
	}
	return m.importAppleServices(ctx, r, realImportVerifier{importAppleClient})
}
func (m *Manager) importAppleServices(ctx context.Context, r ImportRequest, v importVerifier) (ImportResult, bool, error) {
	if err := r.NormalizeAndValidate(); err != nil {
		return ImportResult{}, false, err
	}
	m.importOnce.Do(func() { m.importGate = make(chan struct{}, 1) })
	select {
	case m.importGate <- struct{}{}:
		defer func() { <-m.importGate }()
	case <-ctx.Done():
		return ImportResult{}, false, ImportUnavailable
	}
	m.mu.RLock()
	version := accountsVersion(m.accounts)
	snapshot := make(map[string]*Account, len(m.accounts))
	for id, a := range m.accounts {
		snapshot[id] = copyAccount(a)
	}
	m.mu.RUnlock()

	s, err := v.identity(ctx, r.Host, r.Cookies)
	if err != nil {
		return ImportResult{}, false, importUpstreamError(err, ImportAppleFailed)
	}
	if s.ValidatedAppleID == "" || s.DSID == "" || s.HSAChallengeRequired {
		return ImportResult{}, false, ImportAppleFailed
	}
	if normalizeIdentity(s.ValidatedAppleID) != r.AppleID {
		return ImportResult{}, false, ImportConflict
	}
	list, updated, err := v.directory(ctx, s)
	if err != nil {
		return ImportResult{}, false, importUpstreamError(err, ImportHMEFailed)
	}
	if !verifiedImportMailbox(r.ICloudEmail, list) {
		return ImportResult{}, false, ImportHMEFailed
	}
	if err = v.imap(ctx, r.ICloudEmail, r.AppPassword); err != nil {
		return ImportResult{}, false, importUpstreamError(err, ImportIMAPFailed)
	}

	var target *Account
	for key, a := range snapshot {
		if a == nil || a.ID != key {
			return ImportResult{}, false, ImportConflict
		}
		relevant := a.ImportSourceID == r.SourceID || normalizeIdentity(a.ICloudEmail) == r.ICloudEmail ||
			normalizeIdentity(a.VerifiedAppleID) == r.AppleID || normalizeIdentity(a.RealEmail) == r.AppleID || a.VerifiedDSID == s.DSID
		if !relevant {
			continue
		}
		if a.ImportSourceID != "" {
			if a.ImportSourceID != r.SourceID || normalizeIdentity(a.VerifiedAppleID) != r.AppleID || a.VerifiedDSID != s.DSID ||
				normalizeIdentity(a.ICloudEmail) != r.ICloudEmail || normalizeIdentity(a.RealEmail) != r.AppleID {
				return ImportResult{}, false, ImportConflict
			}
		} else {
			// Local legacy metadata is only a hint, never identity proof. Validate the
			// OLD cookies too before attaching a durable mapping. Expired/ambiguous
			// credentials fail closed without changing the legacy record.
			host := a.Host
			if host == "" {
				host = "icloud.com"
			}
			if host != "icloud.com" && host != "icloud.com.cn" {
				return ImportResult{}, false, ImportConflict
			}
			old, verifyErr := v.identity(ctx, host, cloneCookies(a.Cookies))
			if verifyErr != nil {
				if importUpstreamError(verifyErr, ImportConflict) == ImportUnavailable {
					return ImportResult{}, false, ImportUnavailable
				}
				return ImportResult{}, false, ImportConflict
			}
			if normalizeIdentity(old.ValidatedAppleID) != r.AppleID || old.DSID != s.DSID || old.HSAChallengeRequired || normalizeIdentity(a.ICloudEmail) != r.ICloudEmail {
				return ImportResult{}, false, ImportConflict
			}
		}
		if target != nil || !importIDPattern.MatchString(a.ID) {
			return ImportResult{}, false, ImportConflict
		}
		target = a
	}
	if ctx.Err() != nil {
		return ImportResult{}, false, ImportUnavailable
	}
	created := target == nil
	if created {
		target = &Account{ID: "acc_" + uuid.NewString(), Name: r.Name, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	}
	if target.Name == "" {
		target.Name = r.AppleID
	}
	target.ImportSourceID = r.SourceID
	target.VerifiedAppleID = r.AppleID
	target.VerifiedDSID = s.DSID
	target.ImportRevision++
	target.RealEmail = r.AppleID
	target.ICloudEmail = r.ICloudEmail
	target.Host = r.Host
	// Persist only the cookies used by this Web session. Never create/replace an
	// Apple Account session or change user proxy/mailbox settings.
	target.Cookies = cloneCookies(r.Cookies)
	for _, c := range updated.Cookies {
		if strings.TrimPrefix(c.Domain, ".") == r.Host && c.Path == "/" && !c.HostOnly && (c.Expires.IsZero() || c.Expires.After(time.Now())) {
			target.Cookies[c.Name] = c.Value
		}
	}
	target.AppPassword = r.AppPassword
	target.Status = "active"
	target.LastError = ""
	target.LastValidated = time.Now().UTC().Format(time.RFC3339)
	target.AliasTotal = len(list.Aliases)
	target.AliasActive = 0
	for _, alias := range list.Aliases {
		if alias.IsActive {
			target.AliasActive++
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if accountsVersion(m.accounts) != version {
		return ImportResult{}, false, ImportConflict
	}
	if ctx.Err() != nil {
		return ImportResult{}, false, ImportUnavailable
	}
	previous := m.accounts[target.ID]
	m.accounts[target.ID] = target
	if err := m.save(); err != nil {
		if previous == nil {
			delete(m.accounts, target.ID)
		} else {
			m.accounts[target.ID] = previous
		}
		return ImportResult{}, false, ImportStorageFailed
	}
	return ImportResult{target.ID, r.SourceID, "active", true, true}, created, nil
}

func accountsVersion(accounts map[string]*Account) [32]byte {
	raw, _ := json.Marshal(accounts)
	return sha256.Sum256(raw)
}
func verifiedImportMailbox(email string, list apple.ListResult) bool {
	if !isImportMailboxDomain(email) {
		return false
	}
	// The complete directory distinguishes forwarding destinations from HME
	// aliases. Successful fixed-host IMAP authentication is required as well.
	for _, a := range list.Aliases {
		if normalizeIdentity(a.HME) == email {
			return false
		}
	}
	for _, candidate := range list.ForwardToEmails {
		if normalizeIdentity(candidate) == email {
			return true
		}
	}
	return false
}

func isImportMailboxDomain(email string) bool {
	index := strings.LastIndexByte(email, '@')
	if index < 1 {
		return false
	}
	switch email[index+1:] {
	case "icloud.com", "me.com", "mac.com":
		return true
	}
	return false
}
