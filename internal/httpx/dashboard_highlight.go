// dashboard_highlight.go — search-snippet highlighter (P7-11): matches are
// found in the ORIGINAL text, then every segment is HTML-escaped — a query
// can never match inside an entity like "&lt;", and raw text is never emitted
// (T-07-02-01).
package httpx

import (
	"html/template"
	"strings"
)

// mark escapes text while wrapping case-insensitive matches of query in
// <mark>. Empty query → escaped text only.
func mark(text, query string) template.HTML {
	q := strings.ToLower(query)
	if q == "" {
		return template.HTML(template.HTMLEscapeString(text)) //nolint:gosec // All text is HTML-escaped.
	}
	var b strings.Builder
	lower := strings.ToLower(text)
	for {
		i := strings.Index(lower, q)
		if i < 0 {
			b.WriteString(template.HTMLEscapeString(text))
			break
		}
		b.WriteString(template.HTMLEscapeString(text[:i]))
		b.WriteString("<mark>")
		b.WriteString(template.HTMLEscapeString(text[i : i+len(q)]))
		b.WriteString("</mark>")
		text, lower = text[i+len(q):], lower[i+len(q):]
	}
	return template.HTML(b.String()) //nolint:gosec // Text segments are escaped and mark tags are constant.
}
