package mail

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap"
)

// Keep legacy INBOX IDs numeric; qualify Junk UIDs to prevent cross-folder
// collisions in list keys, detail requests, and deletion requests.
func ParseMessageID(id string) (folder string, uid uint32, err error) {
	folder = "INBOX"
	if strings.HasPrefix(id, "Junk:") {
		folder, id = "Junk", strings.TrimPrefix(id, "Junk:")
	}
	for _, ch := range id {
		if ch < '0' || ch > '9' {
			return "", 0, fmt.Errorf("邮件 ID 无效")
		}
	}
	n, err := strconv.ParseUint(id, 10, 32)
	if err != nil || n == 0 {
		return "", 0, fmt.Errorf("邮件 ID 无效")
	}
	return folder, uint32(n), nil
}

func tagFolder(m *Message, folder string) {
	m.Folder = folder
	if folder != "INBOX" {
		m.ID = folder + ":" + m.ID
	}
}

// FolderReadError prevents failed folder reads from returning partial results
// or silently switching transports after a successful IMAP connection.
type FolderReadError struct{ Err error }

func (e *FolderReadError) Error() string {
	return "读取收件箱或垃圾邮件失败: " + e.Err.Error()
}
func (e *FolderReadError) Unwrap() error { return e.Err }

func (c *Client) readFolders() ([]string, error) {
	if c.cli == nil {
		return nil, fmt.Errorf("未连接")
	}
	folders := []string{"INBOX"}
	boxes := make(chan *imap.MailboxInfo, 4)
	done := make(chan error, 1)
	go func() { done <- c.cli.List("", "Junk", boxes) }()
	for box := range boxes {
		if box == nil || box.Name != "Junk" {
			continue
		}
		selectable := true
		for _, attr := range box.Attributes {
			if strings.EqualFold(attr, "\\Noselect") {
				selectable = false
			}
		}
		if selectable && len(folders) == 1 {
			folders = append(folders, "Junk")
		}
	}
	if err := <-done; err != nil {
		return nil, &FolderReadError{Err: err}
	}
	return folders, nil
}

// ListInbox merges INBOX and iCloud's Junk without moving or marking mail read.
func (c *Client) ListInbox(limit, days int) ([]Message, error) {
	return c.listCombined("", limit, days)
}

func (c *Client) listCombined(recipient string, limit, days int) ([]Message, error) {
	if limit <= 0 {
		limit = 20
	}
	folders, err := c.readFolders()
	if err != nil {
		return nil, err
	}
	out := []Message{}
	for _, folder := range folders {
		if recipient == "" {
			messages, err := c.listFolder(folder, limit, days)
			if err != nil {
				return nil, &FolderReadError{Err: err}
			}
			out = append(out, messages...)
		} else {
			err := c.forEachRecipientFolder(folder, recipient, limit, days, func(m Message) bool {
				tagFolder(&m, folder)
				out = append(out, m)
				return true
			})
			if err != nil {
				return nil, &FolderReadError{Err: err}
			}
		}
	}
	return mergeLatest(out, limit), nil
}

func mergeLatest(messages []Message, limit int) []Message {
	sort.SliceStable(messages, func(i, j int) bool {
		a, _ := time.Parse(time.RFC3339, messages[i].Date)
		b, _ := time.Parse(time.RFC3339, messages[j].Date)
		return a.After(b)
	})
	if limit > 0 && len(messages) > limit {
		messages = messages[:limit]
	}
	return messages
}

func (c *Client) ForEachByRecipient(recipient string, limit, days int, onMsg func(Message) bool) error {
	if onMsg == nil {
		return fmt.Errorf("onMsg 不能为空")
	}
	messages, err := c.FindByRecipient(recipient, limit, days)
	if err != nil {
		return err
	}
	for _, m := range messages {
		if !onMsg(m) {
			break
		}
	}
	return nil
}
