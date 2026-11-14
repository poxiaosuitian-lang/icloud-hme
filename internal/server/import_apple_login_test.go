package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"icloud-hme/internal/account"
)

type integrationLoginFixture struct {
	*fakeBackend
	calls int
}

func (b *integrationLoginFixture) ImportAppleLogin(ctx context.Context, r account.ImportAppleLoginRequest) (account.ImportAppleLoginResult, error) {
	b.calls++
	return account.ImportAppleLoginResult{SourceID: r.SourceID, State: "ready"}, nil
}
func TestImportAppleLoginDedicatedAuthorizationAndShape(t *testing.T) {
	b := &integrationLoginFixture{fakeBackend: &fakeBackend{}}
	s := newWithBackend(b, Config{AdminPassword: "fixture-admin", ImportToken: testImportToken, APIToken: strings.Repeat("s", 40)})
	defer s.Close()
	req := account.ImportAppleLoginRequest{Version: 1, SourceID: account.ImportSourceID("owner@example.test"), AppleID: "owner@example.test", Operation: "login", Password: "fixture-private-password"}
	raw, _ := json.Marshal(req)
	requestNumber := 0
	call := func(token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/integrations/appleservices/apple-login", strings.NewReader(body))
		requestNumber++
		r.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", requestNumber)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	for _, token := range []string{"", strings.Repeat("s", 40), "wrong"} {
		if call(token, string(raw)).Code != 401 {
			t.Fatal("wrong capability accepted")
		}
	}
	if b.calls != 0 {
		t.Fatal("unauthorized Apple access")
	}
	for _, body := range []string{`{"version":1,"version":1}`, strings.TrimSuffix(string(raw), "}") + `,"proxy":"http://example.test"}`, string(raw) + `{}`, strings.Replace(string(raw), `"login"`, `"delete"`, 1)} {
		if call(testImportToken, body).Code != 400 {
			t.Fatal("invalid request accepted")
		}
	}
	if b.calls != 0 {
		t.Fatal("invalid Apple access")
	}
	w := call(testImportToken, string(raw))
	if w.Code != 200 || b.calls != 1 || strings.Contains(w.Body.String(), req.Password) || !strings.Contains(w.Body.String(), `"state":"ready"`) {
		t.Fatal("login integration failed/leaked")
	}
}
