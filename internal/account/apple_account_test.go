package account

import (
	"context"
	"encoding/json"
	"errors"
	"icloud-hme/internal/icloudmail/apple"
	"os"
	"strings"
	"testing"
	"time"
)

func TestAppleSessionIsolationPersistenceAndRedaction(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	sum, err := m.AddAccountWithInput(AddAccountInput{Name: "fixture", ICloudEmail: "owner@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := m.GetAccount(sum.ID)
	session := apple.AccountSession{AppleID: a.loginEmail(), APIKey: "secret-api", SCNT: "secret-scnt", Cookies: []apple.PersistentCookie{{Name: "sid", Value: "secret-cookie", Domain: "apple.com", Path: "/"}}, AuthenticatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), RefreshedAt: time.Now()}
	if err = m.saveAppleSession(a, session); err != nil {
		t.Fatal(err)
	}
	copy, _ := m.GetAccount(a.ID)
	copy.AppleSession.Cookies[0].Value = "mutated"
	saved, _ := m.GetAccount(a.ID)
	if saved.AppleSession.Cookies[0].Value != "secret-cookie" || len(saved.Cookies) != 0 {
		t.Fatal("session mixing or shallow copy")
	}
	for _, value := range []any{m.ListSummaries(), m.ListAccounts()} {
		raw, _ := json.Marshal(value)
		if strings.Contains(string(raw), "secret-") {
			t.Fatal("secret in public listing")
		}
	}
	m2, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	restored, _ := m2.GetAccount(a.ID)
	if restored.AppleSession.APIKey != "secret-api" || !restored.Summary().HasAppleSession {
		t.Fatal("session not restored")
	}
	info, err := os.Stat(m.dataFile)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credential file permissions", err)
	}
	other := "other@example.test"
	if _, err = m.UpdateMetadata(a.ID, UpdateAccountInput{ICloudEmail: &other}); err != nil {
		t.Fatal(err)
	}
	changed, _ := m.GetAccount(a.ID)
	if changed.AppleSession != nil {
		t.Fatal("old identity session retained")
	}
	if err = m.saveAppleSession(a, session); !errors.Is(err, ErrAppleSessionChanged) {
		t.Fatal("stale response saved", err)
	}
}
func TestAppleOTPRequiresPendingLogin(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	a, err := m.AddAccountWithInput(AddAccountInput{Name: "fixture", ICloudEmail: "owner@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.LoginAppleAccount(context.Background(), a.ID, "", "123456"); !errors.Is(err, ErrAppleOTPExpired) {
		t.Fatal(err)
	}
}
