package rewriter

import (
	"strings"
	"testing"
)

func TestCSSRewriter_Rewrite(t *testing.T) {

	domain := "example.com"
	proxyBase := ""

	tests := []struct {
		name     string
		input    string
		contains []string
		excludes []string
	}{
		{
			name:     "url with root path",
			input:    `background: url(/images/bg.png);`,
			contains: []string{`url("/example.com/images/bg.png")`},
		},
		{
			name:     "url with double quotes",
			input:    `background: url("/images/bg.png");`,
			contains: []string{`url("/example.com/images/bg.png")`},
		},
		{
			name:     "url with single quotes",
			input:    `background: url('/images/bg.png');`,
			contains: []string{`url("/example.com/images/bg.png")`},
		},
		{
			name:     "data URI preserved",
			input:    `background: url(data:image/png;base64,abc);`,
			contains: []string{`url("data:image/png;base64,abc")`},
		},
		{
			name:     "multiple url()",
			input:    `background: url(/img1.png), url(/img2.png);`,
			contains: []string{`url("/example.com/img1.png")`, `url("/example.com/img2.png")`},
		},
		{
			name:     "import with url",
			input:    `@import url("/styles/main.css");`,
			contains: []string{`url("/example.com/styles/main.css")`},
		},
		{
			name:     "import string",
			input:    `@import "/styles/main.css";`,
			contains: []string{`@import "/example.com/styles/main.css"`},
		},
		{
			name:     "font-face src",
			input:    `src: url(/fonts/font.woff2) format('woff2');`,
			contains: []string{`url("/example.com/fonts/font.woff2")`},
		},
		{
			name:     "relative url preserved",
			input:    `background: url(../images/bg.png);`,
			contains: []string{`url("../images/bg.png")`},
		},
		{
			name:     "import media query preserved",
			input:    `@import url("print.css") print;`,
			contains: []string{`url("`, `print`},
		},
		{
			name:     "url in comment preserved",
			input:    `/* background: url(/secret.png) */`,
			contains: []string{`/* background: url(/secret.png) */`},
		},
		{
			name:     "local in font-face preserved",
			input:    `src: local('Font Name'), url(font.woff2);`,
			contains: []string{`local('Font Name')`, `url(`},
		},
		{
			name:     "url with escaped parens",
			input:    `background: url(alpha\(beta\).png);`,
			contains: []string{`url("alpha`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := string(RewriteCSS([]byte(tt.input), domain, proxyBase))

			for _, s := range tt.contains {
				if !strings.Contains(result, s) {
					t.Errorf("Expected result to contain %q, got: %s", s, result)
				}
			}

			for _, s := range tt.excludes {
				if strings.Contains(result, s) {
					t.Errorf("Expected result to NOT contain %q, got: %s", s, result)
				}
			}
		})
	}
}
