package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
	"icloud-hme/internal/mail"
)

const serviceTestToken = "test-token-012345678901234567890123456789"

func TestServiceAPIAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name, token, header, query string
		status                     int
	}{
		{"disabled", "", "Bearer " + serviceTestToken, "", 503},
		{"missing", serviceTestToken, "", "", 401},
		{"wrong", serviceTestToken, "Bearer wrong", "", 401},
		{"basic", serviceTestToken, "Basic " + serviceTestToken, "", 401},
		{"query ignored", serviceTestToken, "", "?token=" + serviceTestToken, 401},
		{"extra fields", serviceTestToken, "Bearer " + serviceTestToken + " extra", "", 401},
		{"valid", serviceTestToken, "Bearer " + serviceTestToken, "", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newWithBackend(&fakeBackend{}, Config{AdminPassword: "admin-pass-2026-strong", APIToken: tc.token})
			req := httptest.NewRequest("GET", "/api/v1/accounts"+tc.query, nil)
			req.Header.Set("Authorization", tc.header)
			// Even a cookie does not opt a service request into session authentication.
			req.AddCookie(&http.Cookie{Name: "hme_session", Value: "invalid"})
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("missing no-store")
			}
		})
	}
}

func TestServiceAPIOperations(t *testing.T) {
	f := &fakeBackend{
		accounts: []account.Summary{{ID: "acc_1"}},
		created:  &hme.CreateResult{Email: "alias@icloud.com", Label: "API"},
		inbox:    InboxResult{AccountID: "acc_1", Messages: []mail.Message{{ID: "42", To: "alias@icloud.com"}}, Count: 1, Method: "imap"},
	}
	s := newWithBackend(f, Config{AdminPassword: "admin-pass-2026-strong", APIToken: serviceTestToken})
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{"POST", "/api/v1/aliases", `{"account_id":"acc_1","label":"API"}`, 200},
		{"GET", "/api/v1/aliases?account_id=acc_1", "", 200},
		{"GET", "/api/v1/inbox?account_id=acc_1&alias=alias%40icloud.com&limit=5&days=30", "", 200},
		{"GET", "/api/v1/inbox/42?account_id=acc_1", "", 200},
		{"GET", "/api/v1/inbox/Junk%3A42?account_id=acc_1", "", 200},
		{"GET", "/api/v1/inbox/Trash%3A42?account_id=acc_1", "", 400},
		{"GET", "/api/v1/inbox/Junk%3A0?account_id=acc_1", "", 400},
		{"POST", "/api/v1/aliases", `{"max_aliases":0}`, 400},
		{"GET", "/api/v1/inbox?account_id=acc_1&limit=101", "", 400},
		{"GET", "/api/v1/inbox", "", 400},
		{"GET", "/api/accounts", "", 401},
		{"POST", "/api/create", `{"account_id":"acc_1"}`, 401},
		{"DELETE", "/api/v1/inbox/42?account_id=acc_1", "", 404},
		{"POST", "/api/v1/accounts", "{}", 404},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+serviceTestToken)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if !json.Valid(rec.Body.Bytes()) {
				t.Fatal("expected JSON")
			}
			if tc.method == "POST" && tc.status == 200 && !strings.Contains(rec.Body.String(), "alias@icloud.com") {
				t.Fatal("missing created address")
			}
		})
	}
	if f.listInboxQuery != (InboxQuery{AccountID: "acc_1", Alias: "alias@icloud.com", Limit: 5, Days: 30}) {
		t.Fatalf("wrong filters: %+v", f.listInboxQuery)
	}
}

func TestServiceAPIRejectsWeakConfiguredToken(t *testing.T) {
	for _, token := range []string{"short", strings.Repeat("a", 32) + "\n"} {
		_, err := New(nil, Config{APIToken: token})
		if err == nil {
			t.Fatal("expected token validation error")
		}
	}
}

// Token clients must see the same folder-qualified IDs as the browser; neither
// route may strip Junk or coerce message IDs into integers.
func TestServiceAPIPreservesMergedInbox(t *testing.T) {
	for _, method := range []string{"imap", "web_api"} {
		t.Run(method, func(t *testing.T) {
			f := &fakeBackend{inbox: InboxResult{
				AccountID: "acc_1", Count: 2, Method: method,
				Messages: []mail.Message{
					{ID: "Junk:42", Folder: "Junk"},
					{ID: "42", Folder: "INBOX"},
				},
			}}
			s := newWithBackend(f, Config{AdminPassword: "admin-pass-2026-strong", APIToken: serviceTestToken})
			for _, alias := range []string{"", "&alias=alias%40icloud.com"} {
				req := httptest.NewRequest("GET", "/api/v1/inbox?account_id=acc_1&limit=2&days=7"+alias, nil)
				req.Header.Set("Authorization", "Bearer "+serviceTestToken)
				rec := httptest.NewRecorder()
				s.Handler().ServeHTTP(rec, req)
				var result struct {
					Success bool        `json:"success"`
					Data    InboxResult `json:"data"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if rec.Code != 200 || !result.Success || result.Data.Method != method || result.Data.Count != 2 || len(result.Data.Messages) != 2 {
					t.Fatalf("response=%s", rec.Body.String())
				}
				for i, want := range f.inbox.Messages {
					if got := result.Data.Messages[i]; got.ID != want.ID || got.Folder != want.Folder {
						t.Fatalf("message=%+v want=%+v", got, want)
					}
				}
			}
		})
	}
}
