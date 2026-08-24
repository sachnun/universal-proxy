package rewriter

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/css"
)

// RewriteCSS rewrites a text/css body so all URLs route through the proxy
// base.
func RewriteCSS(body []byte, domain, proxyBase string) []byte {
	input := parse.NewInputBytes(body)
	l := css.NewLexer(input)
	var out bytes.Buffer
	atImport := false

	for {
		tt, data := l.Next()
		if tt == css.ErrorToken {
			break
		}

		switch tt {
		case css.URLToken:
			atImport = false
			out.WriteString(rewriteCSSURL(string(data), domain, proxyBase))
		case css.StringToken:
			if atImport {
				atImport = false
				url := strings.Trim(string(data), "\"'")
				rewritten := ToProxyURL(url, domain, proxyBase)
				out.WriteString(strconv.Quote(rewritten))
			} else {
				out.Write(data)
			}
		case css.AtKeywordToken:
			atImport = string(data) == "@import"
			out.Write(data)
		default:
			if atImport && tt != css.WhitespaceToken {
				atImport = false
			}
			out.Write(data)
		}
	}

	return out.Bytes()
}

func rewriteCSSURL(urlToken, domain, proxyBase string) string {
	if len(urlToken) < 5 || !strings.HasPrefix(urlToken, "url(") {
		return urlToken
	}

	lastParen := strings.LastIndex(urlToken, ")")
	if lastParen < 4 {
		return urlToken
	}

	var inner string
	quoted := false

	if lastParen >= 6 && (urlToken[4] == '"' || urlToken[4] == '\'') {
		quote := urlToken[4]
		if urlToken[lastParen-1] == quote {
			inner = urlToken[5 : lastParen-1]
			quoted = true
		}
	}

	if !quoted {
		inner = strings.TrimSpace(urlToken[4:lastParen])
	}

	rewritten := ToProxyURL(inner, domain, proxyBase)
	return "url(" + strconv.Quote(rewritten) + ")"
}
