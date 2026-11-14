package mail

import (
	stdmail "net/mail"
	"strings"
	"testing"
)

func TestHTMLBodyWithLinks(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"named verification link", `<p>验证 <a href="https://example.com/verify?token=123&amp;next=%2Fhome">点击这里</a></p>`, "验证 点击这里 (https://example.com/verify?token=123&next=%2Fhome)"},
		{"avoid duplicate URL", `<a href="https://example.com">https://example.com</a>`, "https://example.com"},
		{"empty anchor", `<a href="https://example.com"><img src="tracking.gif"></a>`, "https://example.com"},
		{"unquoted uppercase attributes", `<A HREF=https://example.com><b>Open</b></A>`, "Open (https://example.com)"},
		{"discard dangerous destinations", `<a href="javascript:alert(1)">one</a> <a href="data:text/html,evil">two</a> <a href="//example.com">three</a> <a href="/relative">four</a>`, "one two three four"},
		{"ignore invisible HTML", `<head><title>Hidden</title></head><style>p{color:red}</style><script>alert(1)</script><template><a href="https://evil.example">hidden</a></template><p>Hello<br>world</p>`, "Hello\nworld"},
		{"decode entities once", `<p>&lt;img src=x&gt; &amp;lt;literal&amp;gt;</p>`, "<img src=x> &lt;literal&gt;"},
		{"reject control characters", "<a href=\"https://example.com/&#10;bad\">Open</a>", "Open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := htmlBodyWithLinks(tc.raw); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadBodyWithLinks(t *testing.T) {
	raw := "Content-Type: multipart/alternative; boundary=test\r\n\r\n--test\r\nContent-Type: text/html\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n<p><a href=3D\"https://example.com/verify?token=3D123&amp;next=3D%2Fhome\">验证</a></p>\r\n--test--\r\n"
	msg, err := stdmail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	got, err := readBodyWithLinks(msg)
	if err != nil || got != "验证 (https://example.com/verify?token=123&next=%2Fhome)" {
		t.Fatalf("body %q, error %v", got, err)
	}
	// Preview/OTP text stays unchanged and does not include URL tokens.
	msg, _ = stdmail.ReadMessage(strings.NewReader(raw))
	preview, err := readBody(msg)
	if err != nil || preview != "验证" {
		t.Fatalf("preview %q, error %v", preview, err)
	}
}
