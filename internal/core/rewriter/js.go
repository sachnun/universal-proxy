package rewriter

import (
	"strings"

	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/js"
)

// RewriteJS rewrites a javascript body so all URLs route through the proxy
// base.
func RewriteJS(body []byte, domain, proxyBase string) []byte {
	l := js.NewLexer(parse.NewInputBytes(body))

	type frame struct {
		urlArg int
		curArg int
	}
	stack := make([]frame, 0, 8)

	var out strings.Builder
	identPath := make([]string, 0, 4)
	sawDot := false
	afterImport := false
	afterFrom := false

	push := func(urlArg int) {
		stack = append(stack, frame{urlArg: urlArg, curArg: 0})
	}
	pop := func() {
		if len(stack) > 0 {
			stack = stack[:len(stack)-1]
		}
	}
	curArg := func() int {
		if len(stack) > 0 {
			return stack[len(stack)-1].curArg
		}
		return -1
	}
	urlArg := func() int {
		if len(stack) > 0 {
			return stack[len(stack)-1].urlArg
		}
		return -3
	}

	for {
		tt, data := l.Next()
		if tt == js.ErrorToken {
			break
		}

		switch tt {
		case js.IdentifierToken:
			if afterImport && !sawDot {
				afterImport = false
			}
			ident := string(data)
			if sawDot {
				identPath = append(identPath, ident)
			} else {
				identPath = []string{ident}
			}
			sawDot = false
			out.Write(data)

		case js.DotToken:
			sawDot = true
			out.Write(data)

		case js.OpenParenToken:
			if len(identPath) > 0 {
				push(urlArgForPath(identPath))
			} else {
				push(-1)
			}
			sawDot = false
			out.Write(data)

		case js.CloseParenToken:
			pop()
			sawDot = false
			out.Write(data)

		case js.CommaToken:
			if len(stack) > 0 {
				stack[len(stack)-1].curArg++
			}
			sawDot = false
			out.Write(data)

		case js.EqToken:
			if len(identPath) >= 2 &&
				identPath[len(identPath)-1] == "href" &&
				identPath[len(identPath)-2] == "location" {
				push(-2)
			}
			sawDot = false
			out.Write(data)

		case js.StringToken:
			rewrite := afterFrom || afterImport
			afterFrom = false
			afterImport = false
			if !rewrite {
				ua := urlArg()
				if ua == -2 {
					rewrite = true
				} else if ua >= 0 && curArg() == ua {
					rewrite = true
				}
			}
			if rewrite {
				s := string(data)
				quote := s[:1]
				url := s[1 : len(s)-1]
				out.WriteString(quote + ToProxyURL(url, domain, proxyBase) + quote)
			} else {
				out.Write(data)
			}
			if urlArg() == -2 {
				pop()
			}

		case js.TemplateToken:
			s := string(data)
			if !strings.Contains(s, "${") {
				rewrite := afterFrom
				afterFrom = false
				if !rewrite {
					ua := urlArg()
					if ua == -2 {
						rewrite = true
					} else if ua >= 0 && curArg() == ua {
						rewrite = true
					}
				}
				if rewrite {
					quote := s[:1]
					url := s[1 : len(s)-1]
					out.WriteString(quote + ToProxyURL(url, domain, proxyBase) + quote)
				} else {
					out.Write(data)
				}
				if urlArg() == -2 {
					pop()
				}
			} else {
				out.Write(data)
			}

		case js.ImportToken, js.ExportToken:
			identPath = []string{string(data)}
			afterImport = true
			out.Write(data)

		case js.FromToken:
			afterFrom = true
			out.Write(data)

		case js.OpenBraceToken, js.CloseBraceToken,
			js.OpenBracketToken, js.CloseBracketToken,
			js.QuestionToken, js.ArrowToken, js.EllipsisToken:
			afterImport = false
			sawDot = false
			out.Write(data)

		case js.CommentToken, js.LineTerminatorToken,
			js.WhitespaceToken, js.SemicolonToken,
			js.ColonToken, js.NewToken:
			out.Write(data)

		default:
			sawDot = false
			out.Write(data)
		}
	}

	return []byte(out.String())
}

func urlArgForPath(path []string) int {
	if len(path) == 0 {
		return -1
	}
	fn := path[len(path)-1]
	switch fn {
	case "fetch", "Worker", "WebSocket", "EventSource",
		"register", "assign", "replace",
		"sendBeacon", "importScripts", "URL":
		return 0
	case "import":
		return 0
	case "open":
		if len(path) >= 2 && path[len(path)-2] == "window" {
			return 0
		}
		return 1
	case "pushState", "replaceState":
		return 2
	default:
		return -1
	}
}
