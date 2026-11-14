package hme

import (
	"crypto/sha1"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	http "github.com/bogdanfinn/fhttp"
)

func TestDeriveSRPPasswordProtocols(t *testing.T) {
	salt, err := hex.DecodeString("00112233445566778899aabbccddeeff")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		protocol string
		want     string
	}{
		{"s2k", "87cbdeb1b6e8a52753c3fe2d125638d685cf527e1a2d2fedbb4ecdd29dda089e"},
		{"s2k_fo", "dbd6fcd69c0aa3f7b1b16d11abf6e3f8b4183d0fddb646d1250ef814acc14181"},
	}
	for _, tc := range tests {
		t.Run(tc.protocol, func(t *testing.T) {
			got, err := deriveSRPPassword(tc.protocol, "test-password-123", salt, 1000)
			if err != nil {
				t.Fatal(err)
			}
			if hex.EncodeToString(got) != tc.want {
				t.Fatalf("派生结果不符: got %x want %s", got, tc.want)
			}
		})
	}
}

func TestTwoFactorCodeAccepted(t *testing.T) {
	tests := []struct {
		name   string
		status int
		header http.Header
		body   string
		want   bool
	}{
		{"正常 200", 200, http.Header{}, `{}`, true},
		{"传统 204", 204, http.Header{}, ``, true},
		{"409 携带会话令牌仍未完成", 409, http.Header{"X-Apple-Session-Token": {"token"}}, `{}`, false},
		{"409 正文 valid 仍未完成", 409, http.Header{}, `{"securityCode":{"valid":true}}`, false},
		{"409 无效验证码", 409, http.Header{}, `{"securityCode":{"valid":false}}`, false},
		{"401", 401, http.Header{}, `{}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := twoFactorCodeAccepted(tc.status, tc.header, []byte(tc.body)); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestAuthHeadersCarrySessionState(t *testing.T) {
	c := &Client{}
	state := &authState{
		frameId:   "frame-id",
		clientId:  "client-id",
		authAttr:  "auth-attr",
		sessionID: "session-id",
		scnt:      "scnt-value",
	}
	header := c.updateAuthHeaders(http.Header{}, state)
	wants := map[string]string{
		"X-Apple-Widget-Key":        "client-id",
		"X-Apple-ID-Session-Id":     "session-id",
		"scnt":                      "scnt-value",
		"X-Requested-With":          "XMLHttpRequest",
	}
	for key, want := range wants {
		if got := header.Get(key); got != want {
			t.Errorf("%s: got %q want %q", key, got, want)
		}
	}
	for _, forbidden := range []string{"X-Apple-OAuth-State", "X-Apple-OAuth-Client-Id", "Origin", "Referer"} {
		if got := header.Get(forbidden); got != "" {
			t.Errorf("%s 不应发送: %q", forbidden, got)
		}
	}

	continuation := c.continuationAuthHeaders(http.Header{}, state)
	if got := continuation.Get("X-Requested-With"); got != "" {
		t.Errorf("2FA continuation 不应发送 X-Requested-With: %q", got)
	}
}

func TestMintAppleHashcash(t *testing.T) {
	now := time.Date(2026, 9, 16, 6, 0, 0, 0, time.UTC)
	got, err := mintAppleHashcash("challenge", 12, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "1:12:20260916060000:challenge::") {
		t.Fatalf("格式错误: %s", got)
	}
	digest := sha1.Sum([]byte(got))
	if leadingZeroBits(digest[:]) < 12 {
		t.Fatalf("工作量证明不足: %x", digest)
	}
}

func TestDeriveSRPPasswordRejectsInvalidParameters(t *testing.T) {
	if _, err := deriveSRPPassword("unknown", "password", []byte("salt"), 1000); err == nil {
		t.Fatal("未知协议应返回错误")
	}
	if _, err := deriveSRPPassword("s2k", "password", []byte("salt"), 0); err == nil {
		t.Fatal("无效迭代次数应返回错误")
	}
}
