package rewriter

import (
	"strings"
	"testing"
)

func TestHTMLRewriter_Rewrite(t *testing.T) {

	domain := "example.com"
	proxyBase := ""

	tests := []struct {
		name     string
		input    string
		contains []string
		excludes []string
	}{
		{
			name:     "href attribute",
			input:    `<a href="/path/page">Link</a>`,
			contains: []string{`href="/example.com/path/page"`},
		},
		{
			name:     "src attribute",
			input:    `<img src="/images/logo.png">`,
			contains: []string{`src="/example.com/images/logo.png"`},
		},
		{
			name:     "action attribute",
			input:    `<form action="/submit">`,
			contains: []string{`action="/example.com/submit"`},
		},
		{
			name:     "data-src attribute",
			input:    `<img data-src="/lazy/image.jpg">`,
			contains: []string{`data-src="/example.com/lazy/image.jpg"`},
		},
		{
			name:     "absolute URL same domain",
			input:    `<a href="https://example.com/page">Link</a>`,
			contains: []string{`href="/example.com/page"`},
		},
		{
			name:     "protocol-relative URL",
			input:    `<script src="//cdn.example.com/lib.js"></script>`,
			contains: []string{`src="/cdn.example.com/lib.js"`},
		},
		{
			name:     "preserve data URI",
			input:    `<img src="data:image/png;base64,abc">`,
			contains: []string{`src="data:image/png;base64,abc"`},
		},
		{
			name:     "preserve javascript URI",
			input:    `<a href="javascript:void(0)">Click</a>`,
			contains: []string{`href="javascript:void(0)"`},
		},
		{
			name:     "preserve relative URL",
			input:    `<a href="page.html">Link</a>`,
			contains: []string{`href="page.html"`},
		},
		{
			name:     "preserve hash URL",
			input:    `<a href="#section">Link</a>`,
			contains: []string{`href="#section"`},
		},
		{
			name:     "multiple attributes",
			input:    `<a href="/page" data-href="/other"><img src="/img.png"></a>`,
			contains: []string{`href="/example.com/page"`, `data-href="/example.com/other"`, `src="/example.com/img.png"`},
		},
		{
			name:     "formaction on button",
			input:    `<button formaction="/submit">Go</button>`,
			contains: []string{`formaction="/example.com/submit"`},
		},
		{
			name:     "cite on blockquote",
			input:    `<blockquote cite="/source.html">Text</blockquote>`,
			contains: []string{`cite="/example.com/source.html"`},
		},
		{
			name:     "cite on del",
			input:    `<del cite="/reason.html">Removed</del>`,
			contains: []string{`cite="/example.com/reason.html"`},
		},
		{
			name:     "poster on video",
			input:    `<video poster="/thumb.jpg"></video>`,
			contains: []string{`poster="/example.com/thumb.jpg"`},
		},
		{
			name:     "xlink:href preserved",
			input:    `<svg><use xlink:href="/icons.svg#home"></use></svg>`,
			contains: []string{`xlink:href="/example.com/icons.svg#home"`},
		},
		{
			name:     "base tag preserves other attrs",
			input:    `<base href="/" target="_blank">`,
			contains: []string{`target="_blank"`, `href="/example.com/"`},
		},
		{
			name:     "data-url attribute",
			input:    `<div data-url="/api/data">Content</div>`,
			contains: []string{`data-url="/example.com/api/data"`},
		},
		{
			name:     "data-image attribute",
			input:    `<img data-image="/img/lazy.jpg">`,
			contains: []string{`data-image="/example.com/img/lazy.jpg"`},
		},
		{
			name:     "data-background attribute",
			input:    `<div data-background="/bg.jpg">Content</div>`,
			contains: []string{`data-background="/example.com/bg.jpg"`},
		},
		{
			name:     "background on body",
			input:    `<body background="/bg.png">`,
			contains: []string{`background="/example.com/bg.png"`},
		},
		{
			name:     "data on object",
			input:    `<object data="/movie.swf">`,
			contains: []string{`data="/example.com/movie.swf"`},
		},
		{
			name:     "longdesc on img",
			input:    `<img src="pic.jpg" longdesc="/desc.html">`,
			contains: []string{`longdesc="/example.com/desc.html"`},
		},
		{
			name:     "meta og:url",
			input:    `<meta property="og:url" content="https://example.com/page">`,
			contains: []string{`content="/example.com/page"`},
		},
		{
			name:     "meta og:image",
			input:    `<meta property="og:image" content="https://example.com/img.jpg">`,
			contains: []string{`content="/example.com/img.jpg"`},
		},
		{
			name:     "meta twitter:image",
			input:    `<meta name="twitter:image" content="https://example.com/img.jpg">`,
			contains: []string{`content="/example.com/img.jpg"`},
		},
		{
			name:     "meta refresh reversed order",
			input:    `<meta content="5; url=/new-page" http-equiv="refresh">`,
			contains: []string{`url=/example.com/new-page`},
		},
		{
			name:     "meta msapplication-config",
			input:    `<meta name="msapplication-config" content="https://example.com/config.xml">`,
			contains: []string{`content="/example.com/config.xml"`},
		},
		{
			name:     "ping attribute",
			input:    `<a href="/page" ping="/track/click">Link</a>`,
			contains: []string{`ping="/example.com/track/click"`},
		},
		{
			name:     "multi url ping",
			input:    `<a href="/page" ping="/track/click /track/imp">Link</a>`,
			contains: []string{`ping="/example.com/track/click /example.com/track/imp"`},
		},
		{
			name:     "boolean attribute preserved",
			input:    `<input type="checkbox" disabled name="agree">`,
			contains: []string{`disabled`, `name="agree"`},
		},
		{
			name:     "html comment untouched",
			input:    `<!-- <a href="/secret">hidden</a> --><a href="/page">Link</a>`,
			contains: []string{`<!-- <a href="/secret">hidden</a> -->`, `href="/example.com/page"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := string(RewriteHTML([]byte(tt.input), domain, proxyBase))

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

func TestHTMLRewriter_Srcset(t *testing.T) {

	domain := "example.com"
	proxyBase := ""

	tests := []struct {
		name     string
		input    string
		contains []string
	}{
		{
			name:     "with sizes",
			input:    `<img srcset="/small.jpg 1x, /large.jpg 2x">`,
			contains: []string{`/example.com/small.jpg 1x`, `/example.com/large.jpg 2x`},
		},
		{
			name:     "with widths",
			input:    `<img srcset="/img-320.jpg 320w, /img-640.jpg 640w">`,
			contains: []string{`/example.com/img-320.jpg 320w`, `/example.com/img-640.jpg 640w`},
		},
		{
			name:     "imagesrcset on link",
			input:    `<link rel="preload" as="image" imagesrcset="/hero.jpg 1x, /hero-2x.jpg 2x">`,
			contains: []string{`/example.com/hero.jpg 1x`, `/example.com/hero-2x.jpg 2x`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := string(RewriteHTML([]byte(tt.input), domain, proxyBase))

			for _, s := range tt.contains {
				if !strings.Contains(result, s) {
					t.Errorf("Expected result to contain %q, got: %s", s, result)
				}
			}
		})
	}
}

func TestHTMLRewriter_MetaRefresh(t *testing.T) {

	domain := "example.com"
	proxyBase := ""

	input := `<meta http-equiv="refresh" content="5; url=/new-page">`
	result := string(RewriteHTML([]byte(input), domain, proxyBase))

	if !strings.Contains(result, "/example.com/new-page") {
		t.Errorf("Expected meta refresh URL to be rewritten, got: %s", result)
	}
}

func TestHTMLRewriter_InlineStyle(t *testing.T) {

	domain := "example.com"
	proxyBase := ""

	input := `<div style="background: url(/images/bg.png)"></div>`
	result := string(RewriteHTML([]byte(input), domain, proxyBase))

	if !strings.Contains(result, "/example.com/images/bg.png") {
		t.Errorf("Expected inline style URL to be rewritten, got: %s", result)
	}
}

func TestHTMLRewriter_BaseTag(t *testing.T) {

	domain := "example.com"
	proxyBase := ""

	input := `<base href="https://example.com/">`
	result := string(RewriteHTML([]byte(input), domain, proxyBase))

	if !strings.Contains(result, `<base href="/example.com/"`) {
		t.Errorf("Expected base tag to be rewritten, got: %s", result)
	}
}

func TestHTMLRewriter_SVG(t *testing.T) {

	domain := "example.com"
	proxyBase := ""

	input := `<svg><image xlink:href="/img.svg"/></svg>`
	result := string(RewriteHTML([]byte(input), domain, proxyBase))

	if !strings.Contains(result, `xlink:href="/example.com/img.svg"`) {
		t.Errorf("Expected SVG xlink:href to be rewritten and preserved, got: %s", result)
	}
}

func TestHTMLRewriter_VoidElements(t *testing.T) {

	domain := "example.com"
	proxyBase := ""

	input := `<br><hr><input type="text"><img src="/img.png">`
	result := string(RewriteHTML([]byte(input), domain, proxyBase))

	if !strings.Contains(result, `src="/example.com/img.png"`) {
		t.Errorf("Expected img src to be rewritten, got: %s", result)
	}
	if !strings.Contains(result, `<br`) {
		t.Errorf("Expected br to be preserved, got: %s", result)
	}
}

func TestHTMLRewriter_HeadInjection(t *testing.T) {

	domain := "example.com"
	proxyBase := ""

	result := string(RewriteHTML([]byte("<head><title>Test</title></head>"), domain, proxyBase))
	if !strings.Contains(result, "<script>") || !strings.Contains(result, "</script>") {
		t.Errorf("Expected monkey-patch script to be injected after <head>, got: %s", result)
	}
	if !strings.Contains(result, "location.pathname") {
		t.Errorf("Expected monkey-patch to contain URL rewriting logic, got: %s", result)
	}
}

func TestHTMLRewriter_NoHeadNoInjection(t *testing.T) {

	domain := "example.com"
	proxyBase := ""

	result := string(RewriteHTML([]byte("<body><p>No head here</p></body>"), domain, proxyBase))
	if strings.Contains(result, "<script>") {
		t.Errorf("Expected no injection when no <head>, got: %s", result)
	}
}

func TestHTMLRewriter_EmptyAttribute(t *testing.T) {

	domain := "example.com"
	proxyBase := ""

	input := `<a href>Link</a>`
	result := string(RewriteHTML([]byte(input), domain, proxyBase))

	if !strings.Contains(result, `href`) {
		t.Errorf("Expected empty href to be preserved, got: %s", result)
	}
}
