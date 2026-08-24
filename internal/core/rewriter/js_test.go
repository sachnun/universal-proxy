package rewriter

import (
	"strings"
	"testing"
)

func TestJSRewriter_Rewrite(t *testing.T) {

	domain := "example.com"
	proxyBase := ""

	tests := []struct {
		name     string
		input    string
		contains []string
	}{
		{
			name:     "static import with from",
			input:    `import { foo } from '/modules/foo.js';`,
			contains: []string{`from '/example.com/modules/foo.js'`},
		},
		{
			name:     "static import double quotes",
			input:    `import { foo } from "/modules/foo.js";`,
			contains: []string{`from "/example.com/modules/foo.js"`},
		},
		{
			name:     "dynamic import",
			input:    `const mod = import('/modules/lazy.js');`,
			contains: []string{`import('/example.com/modules/lazy.js')`},
		},
		{
			name:     "fetch with root path",
			input:    `fetch('/api/data')`,
			contains: []string{`fetch('/example.com/api/data'`},
		},
		{
			name:     "new URL constructor",
			input:    `new URL('/path/file', base)`,
			contains: []string{`new URL('/example.com/path/file'`},
		},
		{
			name:     "Worker",
			input:    `new Worker('/worker.js')`,
			contains: []string{`new Worker('/example.com/worker.js')`},
		},
		{
			name:     "Worker without new",
			input:    `const w = Worker('/worker.js');`,
			contains: []string{`Worker('/example.com/worker.js')`},
		},
		{
			name:     "EventSource",
			input:    `new EventSource('/events')`,
			contains: []string{`EventSource('/example.com/events')`},
		},
		{
			name:     "serviceWorker.register",
			input:    `navigator.serviceWorker.register('/sw.js')`,
			contains: []string{`.register('/example.com/sw.js')`},
		},
		{
			name:     "XHR open",
			input:    `xhr.open('GET', '/api/data')`,
			contains: []string{`.open('GET', '/example.com/api/data')`},
		},
		{
			name:     "window.open",
			input:    `window.open('/page')`,
			contains: []string{`window.open('/example.com/page')`},
		},
		{
			name:     "location.href assignment",
			input:    `location.href = '/new-page'`,
			contains: []string{`location.href = '/example.com/new-page'`},
		},
		{
			name:     "location.assign",
			input:    `location.assign('/new-page')`,
			contains: []string{`location.assign('/example.com/new-page')`},
		},
		{
			name:     "location.replace",
			input:    `location.replace('/new-page')`,
			contains: []string{`location.replace('/example.com/new-page')`},
		},
		{
			name:     "history.pushState",
			input:    `history.pushState(null, '', '/page')`,
			contains: []string{`history.pushState(null, '', '/example.com/page')`},
		},
		{
			name:     "history.replaceState",
			input:    `history.replaceState(null, '', '/page')`,
			contains: []string{`history.replaceState(null, '', '/example.com/page')`},
		},
		{
			name:     "sendBeacon",
			input:    `navigator.sendBeacon('/log')`,
			contains: []string{`sendBeacon('/example.com/log')`},
		},
		{
			name:     "importScripts",
			input:    `importScripts('/lib.js')`,
			contains: []string{`importScripts('/example.com/lib.js')`},
		},
		{
			name:     "preserve relative import",
			input:    `import { foo } from './foo.js';`,
			contains: []string{`from './foo.js'`},
		},
		{
			name:     "rewrite external URL fetch",
			input:    `fetch('https://api.example.com/data')`,
			contains: []string{`fetch('/api.example.com/data')`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := string(RewriteJS([]byte(tt.input), domain, proxyBase))

			for _, s := range tt.contains {
				if !strings.Contains(result, s) {
					t.Errorf("Expected result to contain %q, got: %s", s, result)
				}
			}
		})
	}
}

func TestJSRewriter_ComplexScript(t *testing.T) {

	domain := "example.com"
	proxyBase := ""

	input := `
import { Component } from '/lib/framework.js';
import '/styles/main.css';

async function loadModule() {
	const mod = await import('/modules/dynamic.js');
	const data = await fetch('/api/users');
	return data.json();
}

const worker = new Worker('/worker.js');
const events = new EventSource('/notifications');
navigator.serviceWorker.register('/sw.js');
const xhr = new XMLHttpRequest();
xhr.open('GET', '/api/data');
window.open('/help');
location.href = '/home';
location.assign('/new-path');
history.pushState(null, '', '/page');
navigator.sendBeacon('/analytics');
importScripts('/utils.js');
`

	result := string(RewriteJS([]byte(input), domain, proxyBase))

	expected := []string{
		"/example.com/lib/framework.js",
		"/example.com/styles/main.css",
		"/example.com/modules/dynamic.js",
		"/example.com/api/users",
		"/example.com/worker.js",
		"/example.com/notifications",
		"/example.com/sw.js",
		"/example.com/api/data",
		"/example.com/help",
		"/example.com/home",
		"/example.com/new-path",
		"/example.com/page",
		"/example.com/analytics",
		"/example.com/utils.js",
	}

	for _, s := range expected {
		if !strings.Contains(result, s) {
			t.Errorf("Expected result to contain %q", s)
		}
	}
}
