package mail

import (
	"net/url"
	"strings"
	"unicode"

	"golang.org/x/net/html"
)

// htmlBodyWithLinks turns HTML into readable text while retaining web links
// such as <a href="https://example.com/verify">Verify</a>. Remote resources and
// scripts are never passed through to the browser.
func htmlBodyWithLinks(raw string) string {
	doc, err := html.Parse(strings.NewReader(raw))
	if err != nil {
		return sanitizePreview(raw)
	}
	var out strings.Builder
	var render func(*html.Node)
	render = func(n *html.Node) {
		if n.Type == html.TextNode {
			out.WriteString(n.Data)
			return
		}
		if n.Type == html.ElementNode {
			switch n.Data {
			case "head", "style", "script", "title", "noscript", "template":
				return
			case "br":
				out.WriteByte('\n')
			case "li":
				out.WriteString("\n- ")
			}
		}
		start := out.Len()
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			render(child)
		}
		if n.Type != html.ElementNode {
			return
		}
		if n.Data == "a" {
			for _, attr := range n.Attr {
				if attr.Key != "href" {
					continue
				}
				href := strings.TrimSpace(attr.Val)
				u, err := url.Parse(href)
				if err != nil || u.Hostname() == "" ||
					(!strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https")) ||
					strings.IndexFunc(href, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
					break
				}
				label := strings.TrimSpace(out.String()[start:])
				if label == "" {
					out.WriteString(href)
				} else if label != href {
					out.WriteString(" (" + href + ")")
				}
				break
			}
		}
		switch n.Data {
		case "p", "div", "tr", "li", "h1", "h2", "h3", "h4", "h5", "h6":
			out.WriteByte('\n')
		}
	}
	render(doc)
	return sanitizePlainPreview(out.String())
}
