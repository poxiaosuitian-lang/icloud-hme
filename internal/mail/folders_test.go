package mail

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend/memory"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-imap/server"
)

func TestParseMessageID(t *testing.T) {
	for _, tc := range []struct {
		id, folder string
		uid        uint32
	}{
		{"1", "INBOX", 1}, {"Junk:1", "Junk", 1}, {"4294967295", "INBOX", 4294967295},
	} {
		folder, uid, err := ParseMessageID(tc.id)
		if err != nil || folder != tc.folder || uid != tc.uid {
			t.Fatalf("parse %q: %q %d %v", tc.id, folder, uid, err)
		}
	}
	for _, id := range []string{"", "0", "Junk:0", "Junk:", "Trash:1", "Junk:1:2", "-1", "+1", " 1", "4294967296", "Junk:4294967296", "../1"} {
		if _, _, err := ParseMessageID(id); err == nil {
			t.Errorf("accepted %q", id)
		}
	}
}

func fixtureMail(uid uint32, subject, recipient string, date time.Time) *memory.Message {
	body := fmt.Sprintf("From: sender@example.org\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nbody %s", recipient, subject, date.Format(time.RFC1123Z), subject)
	return &memory.Message{Uid: uid, Date: date, Size: uint32(len(body)), Body: []byte(body)}
}

func fixtureClient(t *testing.T, withJunk bool) *Client {
	t.Helper()
	b := memory.New()
	u, err := b.Login(nil, "username", "password")
	if err != nil {
		t.Fatal(err)
	}
	inbox, _ := u.GetMailbox("INBOX")
	now := time.Now()
	inbox.(*memory.Mailbox).Messages = []*memory.Message{
		fixtureMail(1, "inbox", "alias@icloud.com", now.Add(-time.Hour)),
	}
	if withJunk {
		if err := u.CreateMailbox("Junk"); err != nil {
			t.Fatal(err)
		}
		junk, _ := u.GetMailbox("Junk")
		junk.(*memory.Mailbox).Messages = []*memory.Message{
			fixtureMail(1, "junk", "alias@icloud.com", now.Add(-30*time.Minute)),
			fixtureMail(2, "other alias", "other@icloud.com", now.Add(-10*time.Minute)),
			fixtureMail(3, "old", "alias@icloud.com", now.Add(-10*24*time.Hour)),
		}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(b)
	srv.AllowInsecureAuth = true
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	cli, err := client.Dial(l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.Login("username", "password"); err != nil {
		t.Fatal(err)
	}
	c := &Client{cli: cli}
	t.Cleanup(c.Disconnect)
	return c
}

func TestCombinedInboxAndJunk(t *testing.T) {
	c := fixtureClient(t, true)
	messages, err := c.FindByRecipient("alias@icloud.com", 20, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].ID != "Junk:1" || messages[0].Folder != "Junk" || messages[1].ID != "1" || messages[1].Folder != "INBOX" {
		t.Fatalf("unexpected merged results: %+v", messages)
	}
	limited, err := c.FindByRecipient("alias@icloud.com", 1, 7)
	if err != nil || len(limited) != 1 || limited[0].ID != "Junk:1" {
		t.Fatalf("limit: %+v %v", limited, err)
	}
	all, err := c.ListInbox(20, 7)
	if err != nil || len(all) != 3 || all[0].ID != "Junk:2" {
		t.Fatalf("unfiltered/date filter: %+v %v", all, err)
	}
	empty, err := c.FindByRecipient("missing@icloud.com", 20, 7)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty: %+v %v", empty, err)
	}
	for _, id := range []string{"1", "Junk:1"} {
		full, err := c.GetFullID(id)
		if err != nil {
			t.Fatal(err)
		}
		subject := "inbox"
		if strings.HasPrefix(id, "Junk:") {
			subject = "junk"
		}
		if full.ID != id || full.Subject != subject || full.Body != "body "+subject {
			t.Fatalf("detail: %+v", full)
		}
		// Listing/detail must not mark either mailbox's UID 1 as read.
		seq := new(imap.SeqSet)
		seq.AddNum(1)
		ch := make(chan *imap.Message, 1)
		done := make(chan error, 1)
		go func() { done <- c.cli.UidFetch(seq, []imap.FetchItem{imap.FetchFlags}, ch) }()
		for m := range ch {
			for _, flag := range m.Flags {
				if flag == imap.SeenFlag {
					t.Fatal("read marked message seen")
				}
			}
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if err := c.DeleteID("Junk:1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetFullID("1"); err != nil {
		t.Fatalf("deleted INBOX UID collision: %v", err)
	}
	if _, err := c.GetFullID("Junk:1"); err == nil {
		t.Fatal("Junk message still present")
	}
}

func TestInboxWithoutJunk(t *testing.T) {
	c := fixtureClient(t, false)
	messages, err := c.ListInbox(20, 7)
	if err != nil || len(messages) != 1 || messages[0].Folder != "INBOX" {
		t.Fatalf("%+v %v", messages, err)
	}
}

func TestMergeLatestTimezones(t *testing.T) {
	messages := []Message{{ID: "1", Date: "2026-09-16T11:00:00+08:00"}, {ID: "Junk:1", Date: "2026-09-16T05:00:00Z"}}
	result := mergeLatest(messages, 1)
	if len(result) != 1 || result[0].ID != "Junk:1" {
		t.Fatalf("%+v", result)
	}
}
