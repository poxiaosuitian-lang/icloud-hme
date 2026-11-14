package server

import (
	"context"
	"errors"
	"testing"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/icloudmail/apple"
)

type channelFake struct {
	batchTestBackend
	appleErr   error
	appleCalls int
}

func (b *channelFake) CreateAliasChannel(ctx context.Context, id, label, channel string) (*hme.CreateResult, error) {
	b.mu.Lock()
	b.appleCalls++
	err := b.appleErr
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return b.batchTestBackend.CreateAlias(id, label)
}
func TestChannelSelectionValidationAndCompatibility(t *testing.T) {
	for _, value := range []string{"bogus", "new", "old"} {
		if _, ok := normalizeCreationChannel(value); ok {
			t.Fatal(value)
		}
	}
	if value, ok := normalizeCreationChannel(""); !ok || value != channelWeb {
		t.Fatal("changed legacy default")
	}
	b := &channelFake{}
	b.accounts = []account.Summary{{ID: "acc_1"}}
	s := newWithBackend(b, Config{AdminPassword: "admin-pass-2026-strong"})
	defer s.Close()
	names, err := s.creationChannels("acc_1", channelAuto)
	if err != nil || len(names) != 1 || names[0] != channelWeb {
		t.Fatal(names, err)
	}
	if _, err = s.creationChannels("acc_1", channelApple); err == nil {
		t.Fatal("new channel without login")
	}
}
func TestAppleErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{
		{account.ErrAppleOTPRequired, "OTP_REQUIRED"}, {account.ErrAppleOTPExpired, "OTP_SESSION_EXPIRED"},
		{apple.ErrInvalidSession, "APPLE_ACCOUNT_LOGIN_REQUIRED"}, {apple.ErrTwoFactorCode, "OTP_INVALID"},
		{apple.ErrRateLimited, "UPSTREAM_RATE_LIMITED"}, {apple.ErrAliasCapacity, "ALIAS_LIMIT_REACHED"},
		{errors.New("secret credential body"), "UPSTREAM_FAILURE"},
	} {
		e := mapAppleError(tc.err)
		if e.Code != tc.code || e.Message == tc.err.Error() {
			t.Fatal(e)
		}
	}
}
