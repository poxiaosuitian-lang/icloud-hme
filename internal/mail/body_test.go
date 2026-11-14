package mail

import (
	"encoding/base64"
	stdmail "net/mail"
	"strings"
	"testing"
)

func TestReadMIMEBody(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"multipart alternative prefers plain", "Content-Type: multipart/alternative; boundary=test\r\n\r\n--test\r\nContent-Type: text/html\r\n\r\n<style>p {font-family:Arial;}</style><p>HTML code</p>\r\n--test\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nCode=3A 123456\r\n--test--\r\n", "Code: 123456"},
		{"base64", "Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString([]byte("验证码 123456")), "验证码 123456"},
		{"html only", "Content-Type: text/html\r\n\r\n<style>p {font-family:Arial;}</style><p>Code 123456</p>", "Code 123456"},
		{"skip attachment", "Content-Type: multipart/mixed; boundary=test\r\n\r\n--test\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=secret.txt\r\n\r\nattachment\r\n--test\r\nContent-Type: text/plain\r\n\r\nbody\r\n--test--\r\n", "body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := stdmail.ReadMessage(strings.NewReader(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			body, err := readBody(m)
			if err != nil || body != tc.want {
				t.Fatalf("got %q %v, want %q", body, err, tc.want)
			}
		})
	}
}
