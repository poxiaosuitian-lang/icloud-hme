package account

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"icloud-hme/internal/icloudmail/apple"
	"icloud-hme/internal/mail"
)

type fakeImportVerifier struct {
	identityFn  func(context.Context, string, map[string]string) (apple.Session, error)
	directoryFn func(context.Context, apple.Session) (apple.ListResult, apple.Session, error)
	imapFn      func(context.Context, string, string) error
}

func (f fakeImportVerifier) identity(c context.Context, h string, v map[string]string) (apple.Session, error) {
	if f.identityFn != nil {
		return f.identityFn(c, h, v)
	}
	return apple.Session{ValidatedAppleID: "owner@example.test", DSID: "12345"}, nil
}
func (f fakeImportVerifier) directory(c context.Context, s apple.Session) (apple.ListResult, apple.Session, error) {
	if f.directoryFn != nil {
		return f.directoryFn(c, s)
	}
	return apple.ListResult{ForwardToEmails: []string{"owner@icloud.com"}, Aliases: []apple.Alias{{HME: "alias@icloud.com", IsActive: true}}}, s, nil
}
func (f fakeImportVerifier) imap(c context.Context, e, p string) error {
	if f.imapFn != nil {
		return f.imapFn(c, e, p)
	}
	return nil
}
func importFixture() ImportRequest {
	return ImportRequest{Version: 1, AppleID: "owner@example.test", SourceID: ImportSourceID("owner@example.test"), Name: "imported", Host: "icloud.com", ICloudEmail: "owner@icloud.com", Cookies: map[string]string{"X-APPLE-WEBAUTH-TOKEN": "synthetic-cookie-secret", "X-APPLE-WEBAUTH-USER": "synthetic-user"}, AppPassword: "abcd-efgh-ijkl-mnop"}
}
func importManager(t *testing.T) *Manager {
	t.Helper()
	m, e := NewManager(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(m.Close)
	return m
}
func mustImport(t *testing.T, m *Manager) (ImportResult, bool) {
	t.Helper()
	r, c, e := m.importAppleServices(context.Background(), importFixture(), fakeImportVerifier{})
	if e != nil {
		t.Fatal(e)
	}
	return r, c
}
func snapshotImport(t *testing.T, m *Manager) ([32]byte, []byte) {
	t.Helper()
	m.mu.RLock()
	defer m.mu.RUnlock()
	raw, e := os.ReadFile(m.dataFile)
	if e != nil {
		t.Fatal(e)
	}
	return accountsVersion(m.accounts), raw
}
func assertUnchanged(t *testing.T, m *Manager, version [32]byte, raw []byte) {
	t.Helper()
	v, b := snapshotImport(t, m)
	if v != version || string(b) != string(raw) {
		t.Fatal("failed import changed data")
	}
}

func TestImportCreateUpdateRestartConcurrent(t *testing.T) {
	m := importManager(t)
	result, created := mustImport(t, m)
	if !created || !importIDPattern.MatchString(result.ID) || !result.HMEVerified || !result.IMAPVerified {
		t.Fatal("invalid create result")
	}
	if r, c := mustImport(t, m); c || r.ID != result.ID {
		t.Fatal("duplicate")
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, c, e := m.importAppleServices(context.Background(), importFixture(), fakeImportVerifier{})
			if e != nil || c || r.ID != result.ID {
				t.Error("concurrent duplicate", e)
			}
		}()
	}
	wg.Wait()
	restarted, e := NewManager(m.dataDir)
	if e != nil {
		t.Fatal(e)
	}
	defer restarted.Close()
	if r, c := mustImport(t, restarted); c || r.ID != result.ID {
		t.Fatal("restart duplicate")
	}
	if len(restarted.ListSummaries()) != 1 {
		t.Fatal("unexpected count")
	}
	st, _ := os.Stat(m.dataFile)
	if st.Mode().Perm() != 0600 {
		t.Fatal("file permissions")
	}
	st, _ = os.Stat(m.dataDir)
	if st.Mode().Perm() != 0700 {
		t.Fatal("directory permissions")
	}
	raw, _ := json.Marshal(result)
	for _, secret := range []string{importFixture().AppPassword, "synthetic-cookie-secret"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("secret in result")
		}
	}
}
func TestImportValidationFailuresAreTransactional(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*ImportRequest)
		verifier fakeImportVerifier
		want     error
	}{
		{name: "version", mutate: func(r *ImportRequest) { r.Version = 2 }, want: ImportInvalid},
		{name: "mailbox suffix attack", mutate: func(r *ImportRequest) { r.ICloudEmail = "owner@icloud.com.attacker.test" }, want: ImportInvalid},
		{name: "host", mutate: func(r *ImportRequest) { r.Host = "127.0.0.1" }, want: ImportInvalid},
		{name: "source", mutate: func(r *ImportRequest) { r.SourceID = "appleservices-bad" }, want: ImportInvalid},
		{name: "cookie header", mutate: func(r *ImportRequest) { r.Cookies["X-APPLE-WEBAUTH-TOKEN"] = "bad\r\nsecret" }, want: ImportInvalid},
		{name: "apple failure", verifier: fakeImportVerifier{identityFn: func(context.Context, string, map[string]string) (apple.Session, error) {
			return apple.Session{}, errors.New("synthetic-cookie-secret")
		}}, want: ImportAppleFailed},
		{name: "identity mismatch", verifier: fakeImportVerifier{identityFn: func(context.Context, string, map[string]string) (apple.Session, error) {
			return apple.Session{ValidatedAppleID: "other@example.test", DSID: "12345"}, nil
		}}, want: ImportConflict},
		{name: "input is not proof", verifier: fakeImportVerifier{identityFn: func(context.Context, string, map[string]string) (apple.Session, error) {
			return apple.Session{AppleID: "owner@example.test", DSID: "12345"}, nil
		}}, want: ImportAppleFailed},
		{name: "hme failure", verifier: fakeImportVerifier{directoryFn: func(context.Context, apple.Session) (apple.ListResult, apple.Session, error) {
			return apple.ListResult{}, apple.Session{}, apple.ErrHMEAuthentication
		}}, want: ImportHMEFailed},
		{name: "alias not mailbox", verifier: fakeImportVerifier{directoryFn: func(_ context.Context, s apple.Session) (apple.ListResult, apple.Session, error) {
			return apple.ListResult{ForwardToEmails: []string{"owner@icloud.com"}, Aliases: []apple.Alias{{HME: "owner@icloud.com"}}}, s, nil
		}}, want: ImportHMEFailed},
		{name: "not forward candidate", verifier: fakeImportVerifier{directoryFn: func(_ context.Context, s apple.Session) (apple.ListResult, apple.Session, error) {
			return apple.ListResult{}, s, nil
		}}, want: ImportHMEFailed},
		{name: "imap failure", verifier: fakeImportVerifier{imapFn: func(context.Context, string, string) error { return mail.ErrIMAPCredentials }}, want: ImportIMAPFailed},
		{name: "imap timeout", verifier: fakeImportVerifier{imapFn: func(context.Context, string, string) error { return mail.ErrIMAPUnavailable }}, want: ImportUnavailable},
		{name: "apple throttle", verifier: fakeImportVerifier{identityFn: func(context.Context, string, map[string]string) (apple.Session, error) {
			return apple.Session{}, &apple.Error{StatusCode: 429}
		}}, want: ImportUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := importManager(t)
			mustImport(t, m)
			version, raw := snapshotImport(t, m)
			r := importFixture()
			if tc.mutate != nil {
				tc.mutate(&r)
			}
			_, _, err := m.importAppleServices(context.Background(), r, tc.verifier)
			if err != tc.want {
				t.Fatalf("got %v want %v", err, tc.want)
			}
			assertUnchanged(t, m, version, raw)
		})
	}
}
func TestImportSaveRollback(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "update"}[existing], func(t *testing.T) {
			m := importManager(t)
			if existing {
				mustImport(t, m)
			} else {
				if e := m.save(); e != nil {
					t.Fatal(e)
				}
			}
			version, raw := snapshotImport(t, m)
			path := m.dataFile
			m.dataFile = filepath.Join(m.dataDir, "unwritable")
			if e := os.Mkdir(m.dataFile, 0700); e != nil {
				t.Fatal(e)
			}
			_, _, e := m.importAppleServices(context.Background(), importFixture(), fakeImportVerifier{})
			if e != ImportStorageFailed {
				t.Fatal(e)
			}
			m.dataFile = path
			assertUnchanged(t, m, version, raw)
		})
	}
}
func TestImportLegacyClaimAndPreserve(t *testing.T) {
	m := importManager(t)
	a := &Account{ID: "legacy_1", Name: "user name", RealEmail: "owner@example.test", ICloudEmail: "owner@icloud.com", Host: "icloud.com", Cookies: map[string]string{"legacy": "credential"}, AppPassword: "old", CreatedAt: "original", Proxy: "http://proxy.test:8080", Mailbox: &MailboxConfig{Email: "external@example.test", Password: "mailbox-secret"}, AppleSession: &apple.AccountSession{APIKey: "second-channel-secret"}}
	m.accounts[a.ID] = a
	if e := m.save(); e != nil {
		t.Fatal(e)
	}
	for _, file := range []string{"alias-usage.json", "dual-schedules.json", "aliases.json"} {
		if e := os.WriteFile(filepath.Join(m.dataDir, file), []byte("synthetic-state"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	var oldVerified bool
	v := fakeImportVerifier{identityFn: func(_ context.Context, _ string, c map[string]string) (apple.Session, error) {
		if c["legacy"] != "" {
			oldVerified = true
		}
		return apple.Session{ValidatedAppleID: "owner@example.test", DSID: "12345"}, nil
	}}
	result, created, e := m.importAppleServices(context.Background(), importFixture(), v)
	if e != nil || created || result.ID != a.ID || !oldVerified {
		t.Fatal("legacy claim failed", e)
	}
	got, _ := m.GetAccount(a.ID)
	if !reflect.DeepEqual(got.AppleSession, a.AppleSession) || !reflect.DeepEqual(got.Mailbox, a.Mailbox) || got.Name != a.Name || got.Proxy != a.Proxy || got.CreatedAt != a.CreatedAt {
		t.Fatal("user configuration lost")
	}
	for _, file := range []string{"alias-usage.json", "dual-schedules.json", "aliases.json"} {
		raw, e := os.ReadFile(filepath.Join(m.dataDir, file))
		if e != nil || string(raw) != "synthetic-state" {
			t.Fatal("related state changed")
		}
	}
}
func TestImportLegacyConflicts(t *testing.T) {
	for _, mode := range []string{"duplicate", "different_identity", "expired", "different_mailbox", "mapped_dsid"} {
		t.Run(mode, func(t *testing.T) {
			m := importManager(t)
			a := &Account{ID: "legacy", RealEmail: "owner@example.test", ICloudEmail: "owner@icloud.com", Cookies: map[string]string{"legacy": "yes"}}
			m.accounts[a.ID] = a
			v := fakeImportVerifier{}
			switch mode {
			case "duplicate":
				b := copyAccount(a)
				b.ID = "legacy2"
				m.accounts[b.ID] = b
			case "different_mailbox":
				a.ICloudEmail = "different@icloud.com"
			case "mapped_dsid":
				a.ImportSourceID = importFixture().SourceID
				a.VerifiedAppleID = importFixture().AppleID
				a.VerifiedDSID = "other-dsid"
			default:
				v.identityFn = func(_ context.Context, _ string, c map[string]string) (apple.Session, error) {
					if c["legacy"] != "" {
						if mode == "expired" {
							return apple.Session{}, apple.ErrInvalidSession
						}
						return apple.Session{ValidatedAppleID: "other@example.test", DSID: "other"}, nil
					}
					return apple.Session{ValidatedAppleID: "owner@example.test", DSID: "12345"}, nil
				}
			}
			if e := m.save(); e != nil {
				t.Fatal(e)
			}
			version, raw := snapshotImport(t, m)
			_, _, e := m.importAppleServices(context.Background(), importFixture(), v)
			if e != ImportConflict {
				t.Fatal(e)
			}
			assertUnchanged(t, m, version, raw)
		})
	}
}
func TestImportConcurrentMutationAndLateCookieSave(t *testing.T) {
	m := importManager(t)
	first, _ := mustImport(t, m)
	old, _ := m.GetAccount(first.ID)
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	v := fakeImportVerifier{imapFn: func(context.Context, string, string) error { close(started); <-release; return nil }}
	go func() { _, _, e := m.importAppleServices(context.Background(), importFixture(), v); done <- e }()
	<-started
	name := "changed during validation"
	if _, e := m.UpdateMetadata(first.ID, UpdateAccountInput{Name: &name}); e != nil {
		t.Fatal(e)
	}
	version, raw := snapshotImport(t, m)
	close(release)
	if e := <-done; e != ImportConflict {
		t.Fatal(e)
	}
	assertUnchanged(t, m, version, raw)
	mustImport(t, m)
	version, raw = snapshotImport(t, m)
	if e := m.SaveCookies(first.ID, map[string]string{"stale": "cookie"}, old.ImportRevision); e == nil {
		t.Fatal("stale refresh overwrote import")
	}
	assertUnchanged(t, m, version, raw)
}
func TestImportSourceIDVector(t *testing.T) {
	// Independent hash test covers case/whitespace normalization and NUL separator.
	if ImportSourceID(" Owner@Example.Test ") != "appleservices-92485be2ec835d00a8e0ddc2c306ccde4bd203353f8bb26a13a7302300d1d392" {
		t.Fatal(ImportSourceID("owner@example.test"))
	}
}

func TestImportUpdatesCredentialsButNotNameOrSecondChannel(t *testing.T) {
	m := importManager(t)
	first, _ := mustImport(t, m)
	m.mu.Lock()
	m.accounts[first.ID].AppleSession = &apple.AccountSession{APIKey: "second-channel-secret"}
	m.mu.Unlock()
	r := importFixture()
	r.Cookies["X-APPLE-WEBAUTH-TOKEN"] = "replacement-cookie-secret"
	r.AppPassword = "qrst-uvwx-yzab-cdef"
	r.Name = "not the user name"
	result, created, err := m.importAppleServices(context.Background(), r, fakeImportVerifier{})
	if err != nil || created || result.ID != first.ID {
		t.Fatal(err)
	}
	a, _ := m.GetAccount(first.ID)
	if a.Cookies["X-APPLE-WEBAUTH-TOKEN"] != "replacement-cookie-secret" || a.AppPassword != r.AppPassword || a.Name != importFixture().Name || a.AppleSession.APIKey != "second-channel-secret" {
		t.Fatal("incorrect credential-only update")
	}
	if err = m.SaveCookies(a.ID, map[string]string{"fresh": "rotated"}, a.ImportRevision); err != nil {
		t.Fatal("current revision blocked", err)
	}
}

func TestImportCancellationLeavesOldAccount(t *testing.T) {
	m := importManager(t)
	mustImport(t, m)
	version, raw := snapshotImport(t, m)
	ctx, cancel := context.WithCancel(context.Background())
	v := fakeImportVerifier{imapFn: func(context.Context, string, string) error { cancel(); return nil }}
	_, _, err := m.importAppleServices(ctx, importFixture(), v)
	if err != ImportUnavailable {
		t.Fatal(err)
	}
	assertUnchanged(t, m, version, raw)
}
