package mail

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func fixtureWebClient(t *testing.T, handler http.HandlerFunc) *WebClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewWebClient(nil, "fixture", "")
	c.mccGatewayURL = srv.URL
	return c
}

func TestWebCombinedInbox(t *testing.T) {
	now := time.Now()
	type thread struct {
		ThreadID  string `json:"threadId"`
		Subject   string `json:"subject"`
		Timestamp int64  `json:"timestamp"`
	}
	folders := map[string][]thread{
		"INBOX": {
			{"1", "ALIAS@example.org inbox", now.Add(-time.Hour).UnixMilli()},
			{"2", "other inbox", now.Add(-10 * time.Minute).UnixMilli()},
		},
		"Junk": {
			{"1", "alias@example.org junk", now.Add(-30 * time.Minute).UnixMilli()},
			{"2", "other junk", now.Add(-5 * time.Minute).UnixMilli()},
			{"3", "alias@example.org old", now.Add(-10 * 24 * time.Hour).UnixMilli()},
			{"4", "alias@example.org undated", 0},
		},
	}
	for _, tc := range []struct {
		name, alias, query string
		limit, days        int
		want               []string
	}{
		{name: "all", limit: 20, days: 7, want: []string{"Junk:2", "2", "Junk:1", "1"}},
		{name: "global limit", limit: 1, days: 7, want: []string{"Junk:2"}},
		{name: "alias across folders", alias: "alias@example.org", limit: 20, days: 7, want: []string{"Junk:1", "1"}},
		{name: "filter before limit", alias: "alias@example.org", limit: 1, days: 7, want: []string{"Junk:1"}},
		{name: "empty", alias: "missing@example.org", limit: 20, days: 7, want: []string{}},
		{name: "default limit", limit: 0, want: []string{"Junk:2", "2", "Junk:1", "1", "Junk:3", "Junk:4"}},
		{name: "negative limit", limit: -1, want: []string{"Junk:2", "2", "Junk:1", "1", "Junk:3", "Junk:4"}},
		{name: "search", query: "a\"b\\c", limit: 1, want: []string{"Junk:2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := []string{}
			c := fixtureWebClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/mailws2/v1/thread/search" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				var payload struct {
					MaxResults     int    `json:"maxResults"`
					Query          string `json:"query"`
					SessionHeaders struct {
						Folder string `json:"folder"`
					} `json:"sessionHeaders"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				folder := payload.SessionHeaders.Folder
				seen = append(seen, folder)
				if payload.Query != tc.query {
					t.Errorf("query=%q", payload.Query)
				}
				expectedLimit := tc.limit
				if expectedLimit <= 0 {
					expectedLimit = 20
				}
				if tc.alias != "" {
					expectedLimit = max(50, expectedLimit*2)
				}
				if payload.MaxResults != expectedLimit {
					t.Errorf("batch=%d want=%d", payload.MaxResults, expectedLimit)
				}
				// Deliberately return unsorted data to test global date ordering.
				_ = json.NewEncoder(w).Encode(map[string]any{"threadList": folders[folder]})
			})
			var messages []Message
			var err error
			if tc.query != "" {
				messages, err = c.SearchMails(tc.query, tc.limit)
			} else {
				messages, err = c.ListInboxFiltered(tc.alias, tc.limit, tc.days)
			}
			if err != nil {
				t.Fatal(err)
			}
			if messages == nil {
				t.Fatal("empty lists must serialize as []")
			}
			if !reflect.DeepEqual(seen, []string{"INBOX", "Junk"}) {
				t.Fatalf("folders=%v", seen)
			}
			ids := []string{}
			for _, m := range messages {
				ids = append(ids, m.ID)
				if m.Folder != "INBOX" && m.Folder != "Junk" {
					t.Errorf("missing folder: %+v", m)
				}
			}
			if !reflect.DeepEqual(ids, tc.want) {
				t.Fatalf("ids=%v want=%v", ids, tc.want)
			}
		})
	}
}

func TestWebFolderFailureNeverReturnsPartialInbox(t *testing.T) {
	for _, folder := range []string{"INBOX", "Junk"} {
		for _, failure := range []string{"http", "api", "json"} {
			t.Run(folder+"/"+failure, func(t *testing.T) {
				c := fixtureWebClient(t, func(w http.ResponseWriter, r *http.Request) {
					var payload struct {
						SessionHeaders struct {
							Folder string `json:"folder"`
						} `json:"sessionHeaders"`
					}
					_ = json.NewDecoder(r.Body).Decode(&payload)
					if payload.SessionHeaders.Folder == folder {
						switch failure {
						case "http":
							http.Error(w, "upstream failed", 502)
						case "api":
							_, _ = w.Write([]byte(`{"success": false}`))
						case "json":
							_, _ = w.Write([]byte(`invalid json`))
						}
						return
					}
					_, _ = w.Write([]byte(`{"threadList":[{"threadId":"1"}]}`))
				})
				messages, err := c.ListInbox(20)
				var folderErr *FolderReadError
				if !errors.As(err, &folderErr) || messages != nil {
					t.Fatalf("partial result: %+v %v", messages, err)
				}
			})
		}
	}
}

func TestWebLegacyEntryPointsIncludeJunk(t *testing.T) {
	for _, name := range []string{"inbox", "alias", "empty search"} {
		t.Run(name, func(t *testing.T) {
			c := fixtureWebClient(t, func(w http.ResponseWriter, r *http.Request) {
				var payload struct {
					SessionHeaders struct {
						Folder string `json:"folder"`
					} `json:"sessionHeaders"`
				}
				_ = json.NewDecoder(r.Body).Decode(&payload)
				if payload.SessionHeaders.Folder == "Junk" {
					_, _ = w.Write([]byte(`{"threadList":[{"threadId":"thread-1","subject":"alias@example.org"}]}`))
				} else {
					_, _ = w.Write([]byte(`{"threadList":[]}`))
				}
			})
			var messages []Message
			var err error
			switch name {
			case "inbox":
				messages, err = c.ListInbox(20)
			case "alias":
				messages, err = c.FindByAlias("alias@example.org", 20)
			default:
				messages, err = c.SearchMails("", 20)
			}
			if err != nil || len(messages) != 1 || messages[0].ID != "Junk:thread-1" || messages[0].Folder != "Junk" {
				t.Fatalf("messages=%+v err=%v", messages, err)
			}
		})
	}
}
