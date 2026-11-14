package account

import "testing"

func TestLoginEmailPrefersConfiguredICloudEmail(t *testing.T) {
	for _, tc := range []struct {
		apple, mailbox, want string
	}{
		{"owner@example.test", "mailbox@icloud.com", "mailbox@icloud.com"},
		{" owner@example.test ", "mailbox@icloud.com", "mailbox@icloud.com"},
		{"", "mailbox@icloud.com", "mailbox@icloud.com"},
		{"  ", " mailbox@icloud.com ", "mailbox@icloud.com"},
		{" owner@example.test ", " ", "owner@example.test"},
		{"", "", ""},
	} {
		a := Account{RealEmail: tc.apple, ICloudEmail: tc.mailbox}
		if got := a.loginEmail(); got != tc.want {
			t.Errorf("loginEmail() = %q, want %q", got, tc.want)
		}
	}
}
