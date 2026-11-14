package hme

import (
	"errors"
	"io"
	"strings"
	"testing"

	http "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
)

type loginRejectedHTTP struct {
	tlsclient.HttpClient
	status int
}

func (h loginRejectedHTTP) Do(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: h.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"secret":"never-expose"}`))}, nil
}

func TestAuthCompleteRejectsFreshLoginNotCookies(t *testing.T) {
	for _, status := range []int{401, 403} {
		c := &Client{httpc: loginRejectedHTTP{status: status}}
		otp, err := c.authComplete(&authState{}, "challenge", "m1", "m2")
		var rejected *LoginRejectedError
		if otp || !errors.As(err, &rejected) || rejected.Status != status {
			t.Fatalf("status=%d otp=%t err=%v", status, otp, err)
		}
		if strings.Contains(err.Error(), "never-expose") {
			t.Fatal("upstream body leaked")
		}
	}
}
