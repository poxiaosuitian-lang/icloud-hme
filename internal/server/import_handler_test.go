package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/account"
)

const testImportToken = "synthetic-import-token-not-for-production-12345"

type importBackend struct {
	*fakeBackend
	calls      int
	err        error
	panicValue string
	created    bool
}

func (b *importBackend) ImportAppleServices(ctx context.Context, r account.ImportRequest) (account.ImportResult, bool, error) {
	b.calls++
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 91*time.Second {
		panic("missing deadline")
	}
	if b.panicValue != "" {
		panic(b.panicValue)
	}
	return account.ImportResult{ID: "acc_fixture", SourceID: r.SourceID, Status: "active", HMEVerified: true, IMAPVerified: true}, b.created, b.err
}
func importServer(t *testing.T) (*Server, *importBackend) {
	t.Helper()
	b := &importBackend{fakeBackend: &fakeBackend{}}
	s := newWithBackend(b, Config{AdminPassword: "synthetic-admin-password", ImportToken: testImportToken, APIToken: strings.Repeat("s", 40)})
	t.Cleanup(s.Close)
	return s, b
}
func importBody() string {
	raw, _ := json.Marshal(account.ImportRequest{Version: 1, SourceID: account.ImportSourceID("owner@example.test"), AppleID: "owner@example.test", Name: "fixture", ICloudEmail: "owner@icloud.com", Host: "icloud.com", Cookies: map[string]string{"X-APPLE-WEBAUTH-TOKEN": "synthetic-cookie-secret", "X-APPLE-WEBAUTH-USER": "user-secret"}, AppPassword: "abcd-efgh-ijkl-mnop"})
	return string(raw)
}
func importHTTP(s *Server, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, importPath, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func TestImportAuthenticationIsolation(t *testing.T) {
	for _, token := range []string{"", "wrong", strings.Repeat("s", 40)} {
		t.Run(token, func(t *testing.T) {
			s, b := importServer(t)
			w := importHTTP(s, importBody(), token)
			if w.Code != 401 || b.calls != 0 {
				t.Fatal(w.Code, b.calls)
			}
			if strings.Contains(w.Body.String(), "secret") {
				t.Fatal("secret returned")
			}
		})
	}
	s, b := importServer(t)
	s.cfg.ImportToken = ""
	if w := importHTTP(s, importBody(), testImportToken); w.Code != 404 || b.calls != 0 {
		t.Fatal("disabled endpoint")
	}
	// Browser login cookies without dedicated bearer are not accepted.
	s, b = importServer(t)
	r := httptest.NewRequest("POST", importPath, strings.NewReader(importBody()))
	r.Header.Set("Cookie", "session=synthetic-session")
	r.Header.Set("Authorization", "Basic "+testImportToken)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 401 || b.calls != 0 {
		t.Fatal("cookie auth accepted")
	}
	s, b = importServer(t)
	for i := 0; i < 5; i++ {
		if w := importHTTP(s, "{}", "wrong"); w.Code != 401 {
			t.Fatal(w.Code)
		}
	}
	if w := importHTTP(s, importBody(), testImportToken); w.Code != 429 || b.calls != 0 || w.Header().Get("Retry-After") == "" {
		t.Fatal("missing failure limit")
	}
}
func TestImportStrictBodyAndBounds(t *testing.T) {
	valid := importBody()
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"unknown proxy", strings.TrimSuffix(valid, "}") + `,"proxy":"http://127.0.0.1"}`, 400},
		{"unknown imap host", strings.TrimSuffix(valid, "}") + `,"imap_host":"127.0.0.1"}`, 400},
		{"duplicate version", strings.TrimSuffix(valid, "}") + `,"version":1}`, 400},
		{"case alias", strings.TrimSuffix(valid, "}") + `,"Version":1}`, 400},
		{"trailing JSON", valid + " {}", 400},
		{"wrong version", strings.Replace(valid, `"version":1`, `"version":2`, 1), 400},
		{"wrong host", strings.Replace(valid, `"host":"icloud.com"`, `"host":"https://icloud.com"`, 1), 400},
		{"oversize", strings.Repeat(" ", importMaxBytes) + valid, 413},
		{"null", `null`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, b := importServer(t)
			w := importHTTP(s, tc.body, testImportToken)
			if w.Code != tc.status || b.calls != 0 {
				t.Fatal(w.Code, b.calls, w.Body.String())
			}
		})
	}
	s, b := importServer(t)
	r := httptest.NewRequest("POST", importPath, strings.NewReader(valid))
	r.Header.Add("Authorization", "Bearer "+testImportToken)
	r.Header.Add("Authorization", "Bearer "+testImportToken)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 401 || b.calls != 0 {
		t.Fatal("duplicate auth accepted")
	}
}
func TestImportSuccessAndStaticErrors(t *testing.T) {
	for _, created := range []bool{false, true} {
		s, b := importServer(t)
		b.created = created
		w := importHTTP(s, importBody(), testImportToken)
		want := 200
		if created {
			want = 201
		}
		if w.Code != want {
			t.Fatal(w.Code)
		}
		var response struct {
			Success bool                 `json:"success"`
			Data    account.ImportResult `json:"data"`
		}
		if e := json.Unmarshal(w.Body.Bytes(), &response); e != nil {
			t.Fatal(e)
		}
		if !response.Success || response.Data.ID != "acc_fixture" || response.Data.SourceID != account.ImportSourceID("owner@example.test") || !response.Data.HMEVerified || !response.Data.IMAPVerified {
			t.Fatal("protocol mismatch")
		}
		if !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
			t.Fatal("cache enabled")
		}
	}
	for _, tc := range []struct {
		err    error
		status int
	}{
		{account.ImportConflict, 409}, {account.ImportAppleFailed, 422}, {account.ImportHMEFailed, 422}, {account.ImportIMAPFailed, 422}, {account.ImportUnavailable, 503}, {account.ImportStorageFailed, 503}, {errors.New("synthetic-cookie-secret"), 503},
	} {
		s, b := importServer(t)
		b.err = tc.err
		w := importHTTP(s, importBody(), testImportToken)
		if w.Code != tc.status || strings.Contains(w.Body.String(), "synthetic-cookie-secret") {
			t.Fatal("error contract", w.Code)
		}
	}
}
func TestImportLogsNeverContainRequestOrPanic(t *testing.T) {
	var captured bytes.Buffer
	oldLog := log.Writer()
	oldGin, oldErr := gin.DefaultWriter, gin.DefaultErrorWriter
	log.SetOutput(&captured)
	gin.DefaultWriter = &captured
	gin.DefaultErrorWriter = &captured
	defer func() { log.SetOutput(oldLog); gin.DefaultWriter = oldGin; gin.DefaultErrorWriter = oldErr }()
	s, b := importServer(t)
	b.panicValue = "synthetic-cookie-secret"
	w := importHTTP(s, importBody(), testImportToken)
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
	r := httptest.NewRequest("POST", importPath+"?password=query-secret", strings.NewReader(importBody()))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+testImportToken)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	for _, secret := range []string{testImportToken, "synthetic-cookie-secret", "abcd-efgh-ijkl-mnop", "query-secret", "owner@example.test"} {
		if strings.Contains(captured.String(), secret) || strings.Contains(w.Body.String(), secret) {
			t.Fatal("credential leak")
		}
	}
}
func TestImportTokenConfiguration(t *testing.T) {
	for _, value := range []string{"short", strings.Repeat("x", 32) + " ", strings.Repeat("x", 32) + "\u2003", strings.Repeat("x", 513)} {
		if ValidateImportToken(value) == nil {
			t.Fatal("invalid token accepted")
		}
	}
	if ValidateImportToken("") != nil || ValidateImportToken(testImportToken) != nil || ValidateImportToken("s1xKey") != nil {
		t.Fatal("valid token rejected")
	}
}

func TestImportAcceptsSixCharacterKey(t *testing.T) {
	s, b := importServer(t)
	s.cfg.ImportToken = "s1xKey"
	if w := importHTTP(s, importBody(), "wrong6"); w.Code != 401 || b.calls != 0 {
		t.Fatal("incorrect six-character key accepted")
	}
	if w := importHTTP(s, importBody(), "s1xKey"); w.Code != 200 || b.calls != 1 {
		t.Fatal("correct six-character key rejected", w.Code)
	}
}
