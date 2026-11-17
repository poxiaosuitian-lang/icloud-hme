package account

import (
	"context"
	"icloud-hme/internal/icloudmail/apple"
	"reflect"
	"testing"
)

func TestWebRecoveryUpdatesOnlyBoundCookies(t *testing.T) {
	m := importManager(t)
	result, _ := mustImport(t, m)
	before, _ := m.GetAccount(result.ID)
	binding, _ := m.WebRecoveryBinding(result.ID)
	r := WebRecoveryRequest{Version: 1, SourceID: binding.SourceID, AppleID: before.VerifiedAppleID, Host: before.Host, Cookies: map[string]string{"X-APPLE-WEBAUTH-TOKEN": "new-fixture-token", "X-APPLE-WEBAUTH-USER": "fixture-user"}}
	update, err := m.prepareWebRecovery(context.Background(), binding, r, fakeImportVerifier{imapFn: func(context.Context, string, string) error { t.Fatal("recovery must not access IMAP"); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	unchanged, _ := m.GetAccount(result.ID)
	if !reflect.DeepEqual(before, unchanged) {
		t.Fatal("prepare mutated account")
	}
	if err = m.CommitWebRecovery(update); err != nil {
		t.Fatal(err)
	}
	if m.SaveCookies(result.ID, before.Cookies, before.ImportRevision, before.WebRecoveryRevision) == nil {
		t.Fatal("stale in-flight cookies overwrote recovered session")
	}
	after, _ := m.GetAccount(result.ID)
	if after.Cookies["X-APPLE-WEBAUTH-TOKEN"] != "new-fixture-token" || after.WebRecoveryRevision != before.WebRecoveryRevision+1 {
		t.Fatal("cookies not applied")
	}
	after.Cookies = before.Cookies
	after.WebRecoveryRevision = before.WebRecoveryRevision
	after.LastValidated = before.LastValidated
	if !reflect.DeepEqual(before, after) {
		t.Fatal("recovery changed account configuration")
	}
	if m.CommitWebRecovery(update) == nil {
		t.Fatal("stale update applied twice")
	}
}
func TestWebRecoveryRejectsForeignIdentityAndConcurrentUpdate(t *testing.T) {
	m := importManager(t)
	result, _ := mustImport(t, m)
	a, _ := m.GetAccount(result.ID)
	binding, _ := m.WebRecoveryBinding(result.ID)
	r := WebRecoveryRequest{Version: 1, SourceID: binding.SourceID, AppleID: a.VerifiedAppleID, Host: a.Host, Cookies: a.Cookies}
	version, raw := snapshotImport(t, m)
	foreign := fakeImportVerifier{identityFn: func(context.Context, string, map[string]string) (apple.Session, error) {
		return apple.Session{ValidatedAppleID: a.VerifiedAppleID, DSID: "other"}, nil
	}}
	if _, err := m.prepareWebRecovery(context.Background(), binding, r, foreign); err == nil {
		t.Fatal("foreign DSID accepted")
	}
	assertUnchanged(t, m, version, raw)
	update, err := m.prepareWebRecovery(context.Background(), binding, r, fakeImportVerifier{})
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.accounts[result.ID].Name = "concurrent edit"
	m.mu.Unlock()
	if m.CommitWebRecovery(update) == nil {
		t.Fatal("concurrent edit overwritten")
	}
	if _, ok := m.WebRecoveryBinding("legacy-unbound"); ok {
		t.Fatal("legacy identity accepted")
	}
}
