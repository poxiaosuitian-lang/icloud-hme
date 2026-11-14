package mail

import (
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	stdmail "net/mail"
	"strings"

	"github.com/emersion/go-message/charset"
)

// readBody decodes MIME parts before sanitizing. In particular, treating an
// entire multipart message as plain text can mistake its HTML stylesheet for
// the whole body and discard a valid verification message.
func readBody(msg *stdmail.Message) (string, error) {
	text, _, err := readMIMEBody(msg.Header, io.LimitReader(msg.Body, 16<<20), 0, false)
	return text, err
}

// Details retain HTML anchor destinations without changing inbox previews or
// OTP extraction. The frontend still receives text, not executable HTML.
func readBodyWithLinks(msg *stdmail.Message) (string, error) {
	text, _, err := readMIMEBody(msg.Header, io.LimitReader(msg.Body, 16<<20), 0, true)
	return text, err
}

func readMIMEBody(header stdmail.Header, body io.Reader, depth int, preserveLinks bool) (string, bool, error) {
	if depth > 12 {
		return "", false, fmt.Errorf("邮件 MIME 嵌套过深")
	}
	ct := header.Get("Content-Type")
	if ct == "" {
		ct = "text/plain"
	}
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return "", false, err
	}
	disposition, _, _ := mime.ParseMediaType(header.Get("Content-Disposition"))
	if disposition == "attachment" {
		return "", false, nil
	}
	switch strings.ToLower(strings.TrimSpace(header.Get("Content-Transfer-Encoding"))) {
	case "base64":
		body = base64.NewDecoder(base64.StdEncoding, body)
	case "quoted-printable":
		body = quotedprintable.NewReader(body)
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		if params["boundary"] == "" {
			return "", false, fmt.Errorf("邮件 MIME boundary 缺失")
		}
		reader := multipart.NewReader(body, params["boundary"])
		var fallback string
		for {
			part, err := reader.NextRawPart()
			if err == io.EOF {
				return fallback, false, nil
			}
			if err != nil {
				return "", false, err
			}
			text, plain, err := readMIMEBody(stdmail.Header(part.Header), part, depth+1, preserveLinks)
			if err != nil {
				return "", false, err
			}
			if plain && text != "" {
				return text, true, nil
			}
			if fallback == "" {
				fallback = text
			}
			_ = part.Close()
		}
	}
	if mediaType != "text/plain" && mediaType != "text/html" {
		return "", false, nil
	}
	if cs := params["charset"]; cs != "" && !strings.EqualFold(cs, "utf-8") && !strings.EqualFold(cs, "us-ascii") {
		body, err = charset.Reader(cs, body)
		if err != nil {
			return "", false, err
		}
	}
	raw, err := io.ReadAll(io.LimitReader(body, (8<<20)+1))
	if err != nil {
		return "", false, err
	}
	if len(raw) > 8<<20 {
		return "", false, fmt.Errorf("邮件正文过大")
	}
	if mediaType == "text/html" {
		if preserveLinks {
			return htmlBodyWithLinks(string(raw)), false, nil
		}
		return sanitizePreview(string(raw)), false, nil
	}
	return sanitizePlainPreview(string(raw)), true, nil
}
