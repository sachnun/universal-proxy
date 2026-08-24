package rewriter

import (
	"bytes"
	ht "html"
	"strings"

	"golang.org/x/net/html"
)

// RewriteHTML rewrites a text/html body so all URLs route through the
// proxy base.
func RewriteHTML(body []byte, domain, proxyBase string) []byte {
	z := html.NewTokenizer(bytes.NewReader(body))
	var out bytes.Buffer

	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return out.Bytes()
		case html.StartTagToken, html.SelfClosingTagToken:
			rewriteTag(z, tt, domain, proxyBase, &out)
		default:
			out.Write(z.Raw())
		}
	}
}

type htmlAttr struct {
	name  string
	value string
}

func rewriteTag(z *html.Tokenizer, tt html.TokenType, domain, proxyBase string, out *bytes.Buffer) {
	tagNameB, hasAttr := z.TagName()
	tagName := string(tagNameB)

	if !hasAttr {
		out.Write(z.Raw())
		if tagName == "head" && tt == html.StartTagToken {
			writeMonitorPatch(out)
		}
		return
	}

	var attrs []htmlAttr
	for hasAttr {
		var k, v []byte
		k, v, hasAttr = z.TagAttr()
		attrs = append(attrs, htmlAttr{string(k), string(v)})
	}

	rewriteSet := make(map[int]bool)
	for i, a := range attrs {
		if shouldRewriteAttr(tagName, a.name, a.value) {
			rewriteSet[i] = true
		}
	}

	metaIdx := checkMetaRule(tagName, attrs)
	if metaIdx >= 0 {
		rewriteSet[metaIdx] = true
	}

	if len(rewriteSet) == 0 {
		out.Write(z.Raw())
		if tagName == "head" && tt == html.StartTagToken {
			writeMonitorPatch(out)
		}
		return
	}

	out.WriteByte('<')
	out.Write(tagNameB)
	for i, a := range attrs {
		out.WriteByte(' ')
		if rewriteSet[i] {
			newVal := rewriteHTMLURL(tagName, attrs, a, domain, proxyBase)
			writeAttr(out, a.name, newVal)
		} else {
			writeAttr(out, a.name, a.value)
		}
	}
	if tt == html.SelfClosingTagToken {
		out.WriteString(" />")
	} else {
		out.WriteByte('>')
	}
	if tagName == "head" && tt == html.StartTagToken {
		writeMonitorPatch(out)
	}
}

var urlAttrs = map[string]bool{
	"src": true, "href": true, "action": true, "cite": true,
	"data": true, "formaction": true, "poster": true,
	"longdesc": true, "background": true, "ping": true,
	"srcset": true, "imagesrcset": true,
	"xlink:href": true,
	"style":      true,
	"data-src":   true, "data-href": true,
	"data-url": true, "data-image": true, "data-background": true,
	"data-endpoint": true, "data-lazy": true,
	"data-original": true, "data-load": true,
	"data-srcset": true, "data-poster": true,
}

func shouldRewriteAttr(tagName, attrName, attrValue string) bool {
	if urlAttrs[attrName] {
		return true
	}
	if strings.HasPrefix(attrName, "data-") && looksLikeURL(attrValue) {
		return true
	}
	return false
}

func looksLikeURL(v string) bool {
	return strings.HasPrefix(v, "/") ||
		strings.HasPrefix(v, "http://") ||
		strings.HasPrefix(v, "https://") ||
		strings.HasPrefix(v, "//")
}

var metaURLNames = map[string]bool{
	"twitter:url": true, "twitter:image": true,
	"msapplication-config": true,
}

func checkMetaRule(tagName string, attrs []htmlAttr) int {
	if tagName != "meta" {
		return -1
	}

	var httpEquiv, property, name string
	contentIdx := -1

	for i, a := range attrs {
		switch a.name {
		case "content":
			contentIdx = i
		case "http-equiv":
			httpEquiv = strings.ToLower(a.value)
		case "property":
			property = strings.ToLower(a.value)
		case "name":
			name = strings.ToLower(a.value)
		}
	}

	if contentIdx < 0 {
		return -1
	}

	if httpEquiv == "refresh" {
		return contentIdx
	}
	if strings.HasPrefix(property, "og:") || strings.HasPrefix(property, "twitter:") {
		return contentIdx
	}
	if metaURLNames[name] {
		return contentIdx
	}
	if strings.HasPrefix(name, "citation_") {
		return contentIdx
	}
	return -1
}

func rewriteHTMLURL(tagName string, attrs []htmlAttr, a htmlAttr, domain, proxyBase string) string {
	switch a.name {
	case "srcset", "imagesrcset":
		return rewriteImageSrcset(a.value, domain, proxyBase)
	case "ping":
		return rewritePingURLs(a.value, domain, proxyBase)
	case "style":
		return string(RewriteCSS([]byte(a.value), domain, proxyBase))
	case "content":
		if tagName == "meta" {
			return rewriteMetaContent(attrs, a, domain, proxyBase)
		}
		return ToProxyURL(a.value, domain, proxyBase)
	default:
		return ToProxyURL(a.value, domain, proxyBase)
	}
}

func rewriteImageSrcset(srcset, domain, proxyBase string) string {
	entries := strings.Split(srcset, ",")
	var result []string
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Fields(entry)
		if len(parts) == 0 {
			continue
		}
		url := parts[0]
		newURL := ToProxyURL(url, domain, proxyBase)
		if len(parts) > 1 {
			result = append(result, newURL+" "+strings.Join(parts[1:], " "))
		} else {
			result = append(result, newURL)
		}
	}
	return strings.Join(result, ", ")
}

func rewritePingURLs(ping, domain, proxyBase string) string {
	urls := strings.Fields(ping)
	var result []string
	for _, u := range urls {
		result = append(result, ToProxyURL(u, domain, proxyBase))
	}
	return strings.Join(result, " ")
}

func rewriteMetaContent(attrs []htmlAttr, a htmlAttr, domain, proxyBase string) string {
	var httpEquiv string
	for _, attr := range attrs {
		if attr.name == "http-equiv" {
			httpEquiv = strings.ToLower(attr.value)
			break
		}
	}
	if httpEquiv == "refresh" {
		lower := strings.ToLower(a.value)
		urlIdx := strings.Index(lower, "url=")
		if urlIdx < 0 {
			return a.value
		}
		prefix := a.value[:urlIdx+4]
		urlPart := strings.TrimSpace(a.value[urlIdx+4:])
		urlPart = strings.Trim(urlPart, `"'`)
		return prefix + ToProxyURL(urlPart, domain, proxyBase)
	}
	return ToProxyURL(a.value, domain, proxyBase)
}

func writeAttr(out *bytes.Buffer, name, value string) {
	if value == "" {
		out.WriteString(name)
		return
	}
	out.WriteString(name)
	out.WriteString(`="`)
	out.WriteString(ht.EscapeString(value))
	out.WriteByte('"')
}

func writeMonitorPatch(out *bytes.Buffer) {
	out.WriteString("<script>")
	out.WriteString(MonitorPatch)
	out.WriteString("</script>")
}
