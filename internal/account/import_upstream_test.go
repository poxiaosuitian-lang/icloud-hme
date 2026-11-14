package account

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"icloud-hme/internal/icloudmail/apple"
)

type importRoundTripFunc func(*http.Request) (*http.Response, error)

func (f importRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type syntheticImportVerifier struct {
	realImportVerifier
	imapCalled *bool
}

func (v syntheticImportVerifier) imap(context.Context, string, string) error {
	*v.imapCalled = true
	return nil
}
func TestImportRealAppleClientSyntheticResponses(t *testing.T) {
	for _, tc := range []struct {
		name, identity, service, directory string
		appleStatus                        int
		want                               error
		hmeCalled, imapCalled              bool
	}{
		{name: "complete", identity: `"appleId":"Owner@Example.Test",`, hmeCalled: true, imapCalled: true},
		{name: "missing response identity", identity: ``, want: ImportAppleFailed},
		{name: "different response identity", identity: `"appleId":"other@example.test",`, want: ImportConflict},
		{name: "hme denied", identity: `"appleId":"owner@example.test",`, directory: `{"success":false}`, want: ImportHMEFailed, hmeCalled: true},
		{name: "incomplete list", identity: `"appleId":"owner@example.test",`, directory: `{"success":true,"result":{"hmeEmails":[],"total":1,"forwardToEmails":["owner@icloud.com"]}}`, want: ImportHMEFailed, hmeCalled: true},
		{name: "ssrf service", identity: `"appleId":"owner@example.test",`, service: "https://127.0.0.1/admin", want: ImportHMEFailed},
		{name: "temporary failure", appleStatus: 503, want: ImportUnavailable},
		{name: "expired cookies", appleStatus: 401, want: ImportAppleFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := importManager(t)
			hmeCalled, imapCalled := false, false
			client, err := apple.NewClient(apple.Config{Transport: importRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				status := 200
				headers := http.Header{}
				body := ""
				switch {
				case strings.HasSuffix(r.URL.Path, "/validate"):
					if r.Method != "POST" || r.URL.Host != "setup.icloud.com" {
						t.Fatal("unexpected validate target")
					}
					cookie, e := r.Cookie("X-APPLE-WEBAUTH-TOKEN")
					if e != nil || cookie.Value != "synthetic-cookie-secret" {
						t.Fatal("cookie not propagated")
					}
					service := tc.service
					if service == "" {
						service = "https://p01-maildomainws.icloud.com"
					}
					body = `{"dsInfo":{` + tc.identity + `"dsid":"12345","primaryEmail":"owner@icloud.com"},"webservices":{"premiummailsettings":{"url":"` + service + `"}}}`
					if tc.appleStatus != 0 {
						status = tc.appleStatus
					}
					headers.Add("Set-Cookie", "X-APPLE-WEBAUTH-TOKEN=rotated-synthetic-secret; Domain=icloud.com; Path=/; Secure; HttpOnly")
				case strings.HasSuffix(r.URL.Path, "/v2/hme/list"):
					if r.Method != "GET" || r.URL.Host != "p01-maildomainws.icloud.com" {
						t.Fatal("unexpected directory target")
					}
					hmeCalled = true
					body = tc.directory
					if body == "" {
						body = `{"success":true,"result":{"hmeEmails":[],"forwardToEmails":["owner@icloud.com","owner@example.test"],"selectedForwardTo":"owner@example.test"}}`
					}
				default:
					t.Fatal("unexpected network operation")
				}
				return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			result, _, err := m.importAppleServices(context.Background(), importFixture(), syntheticImportVerifier{realImportVerifier{client}, &imapCalled})
			if err != tc.want || hmeCalled != tc.hmeCalled || imapCalled != tc.imapCalled {
				t.Fatalf("error=%v HME=%v IMAP=%v", err, hmeCalled, imapCalled)
			}
			if err == nil {
				a, _ := m.GetAccount(result.ID)
				if a.Cookies["X-APPLE-WEBAUTH-TOKEN"] != "rotated-synthetic-secret" {
					t.Fatal("rotated cookies lost")
				}
			} else if len(m.ListSummaries()) != 0 {
				t.Fatal("failed candidate persisted")
			}
		})
	}
}
