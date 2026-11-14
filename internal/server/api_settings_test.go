package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAPISettingsLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api-settings.json")
	s := newWithBackend(&fakeBackend{}, Config{AdminPassword: "admin-pass-2026-strong", APIToken: serviceTestToken, APISettingsFile: path})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	cookie, csrf := login(t, ts, "admin-pass-2026-strong")
	call := func(method, path, body string, session, withCSRF bool) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if session {
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie})
		}
		if withCSRF {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		status, raw, _ := do(t, req)
		var data map[string]any
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			t.Fatal(err)
		}
		return status, data
	}
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/settings/api", ""},
		{"GET", "/api/settings/api/usage", ""},
		{"PUT", "/api/settings/api", `{"enabled":false}`},
		{"POST", "/api/settings/api/token", ""},
		{"POST", "/api/settings/api/rotate", ""},
	} {
		status, _ := call(tc.method, tc.path, tc.body, false, true)
		if status != 401 {
			t.Fatalf("anonymous %s: %d", tc.path, status)
		}
		if tc.method != "GET" {
			status, _ = call(tc.method, tc.path, tc.body, true, false)
			if status != 403 {
				t.Fatalf("missing csrf %s: %d", tc.path, status)
			}
		}
	}
	status, data := call("GET", "/api/settings/api", "", true, false)
	if status != 200 || data["data"].(map[string]any)["has_token"] != true {
		t.Fatal("missing settings")
	}
	raw, _ := json.Marshal(data)
	if strings.Contains(string(raw), serviceTestToken) {
		t.Fatal("settings leaked token")
	}
	status, data = call("POST", "/api/settings/api/token", "", true, true)
	if status != 200 || data["data"].(map[string]any)["token"] != serviceTestToken {
		t.Fatal("reveal failed")
	}
	status, _ = call("PUT", "/api/settings/api", `{"token":"short"}`, true, true)
	if status != 400 || s.apiSettings.snapshot().Token != serviceTestToken {
		t.Fatal("invalid update changed token")
	}
	status, data = call("POST", "/api/settings/api/rotate", "", true, true)
	if status != 200 {
		t.Fatal("rotation failed")
	}
	newToken := data["data"].(map[string]any)["token"].(string)
	if newToken == serviceTestToken || !validAPIToken(newToken) {
		t.Fatal("bad generated token")
	}
	checkBearer := func(token string, want int) {
		t.Helper()
		req, _ := http.NewRequest("GET", ts.URL+"/api/v1/accounts", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		code, _, _ := do(t, req)
		if code != want {
			t.Fatalf("bearer status=%d want=%d", code, want)
		}
	}
	checkBearer(serviceTestToken, 401)
	checkBearer(newToken, 200)
	loaded, err := loadAPISettings(path, serviceTestToken)
	if err != nil || loaded.Token != newToken || !loaded.Enabled {
		t.Fatal("persistence failed")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("insecure file permissions")
	}
	status, _ = call("PUT", "/api/settings/api", `{"enabled":false}`, true, true)
	if status != 200 {
		t.Fatal("disable failed")
	}
	checkBearer(newToken, 503)
	// A restart reads the file rather than falling back to the old env token.
	restarted, err := New(nil, Config{AdminUsername: "admin", AdminPassword: "admin-pass-2026-strong", APIToken: serviceTestToken, APISettingsFile: path})
	if err != nil || restarted.apiSettings.snapshot().Enabled || restarted.apiSettings.snapshot().Token != newToken {
		t.Fatal("restart lost settings")
	}
	status, _ = call("POST", "/api/settings/api/rotate", "", true, true)
	if status != 200 || s.apiSettings.snapshot().Enabled {
		t.Fatal("rotation enabled disabled API")
	}
	status, _ = call("PUT", "/api/settings/api", `{"enabled":true,"token":"`+serviceTestToken+`"}`, true, true)
	if status != 200 {
		t.Fatal("custom token save failed")
	}
	checkBearer(serviceTestToken, 200)
	// Persistence failure must not publish an in-memory change.
	s.apiSettings.path = filepath.Join(path, "invalid-directory", "settings.json")
	status, _ = call("PUT", "/api/settings/api", `{"enabled":false}`, true, true)
	if status != 500 {
		t.Fatal("expected persistence failure")
	}
	checkBearer(serviceTestToken, 200)
}

func TestLoadAPISettingsRejectsInvalidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api-settings.json")
	for _, raw := range []string{`oops`, `{"enabled":true}`, `{"token":"short"}`} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadAPISettings(path, serviceTestToken); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
}
