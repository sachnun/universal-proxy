package main

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"regexp"
	"strings"
	"sync"
)

var (
	regexPool = sync.Pool{
		New: func() any {
			return &regexCache{}
		},
	}
)

type regexCache struct {
	absoluteURL *regexp.Regexp
	attrURL     *regexp.Regexp
	cssURL      *regexp.Regexp
}

func getRegexCache(domain string) *regexCache {
	cache := regexPool.Get().(*regexCache)

	if cache.absoluteURL == nil {
		cache.absoluteURL = regexp.MustCompile(`https?://` + regexp.QuoteMeta(domain) + `([^\s"'<>]*)`)
		cache.attrURL = regexp.MustCompile(`(src|href|action|srcset|data-src|data-href)="([^"]*)"`)
		cache.cssURL = regexp.MustCompile(`url\(([^)]*)\)`)
	}

	return cache
}

func putRegexCache(cache *regexCache) {
	regexPool.Put(cache)
}

func main() {
	log.Println("Server running on :8080")
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Println("Request:", r.URL.Path)
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if len(parts) < 1 {
			log.Println("Invalid path:", r.URL.Path)
			http.Error(w, "Invalid path", 400)
			return
		}
		domain := parts[0]
		remainingPath := "/" + strings.Join(parts[1:], "/")
		log.Printf("Proxying %s to https://%s%s\n", r.URL.Path, domain, remainingPath)
		proxy := &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				req.URL.Scheme = "https"
				req.URL.Host = domain
				req.URL.Path = remainingPath
				req.Host = domain
			},
			ModifyResponse: func(resp *http.Response) error {
				contentType := resp.Header.Get("Content-Type")
				if strings.Contains(contentType, "text/html") {
					body, err := io.ReadAll(resp.Body)
					if err != nil {
						return err
					}
					resp.Body.Close()

					html := string(body)

					cache := getRegexCache(domain)
					defer putRegexCache(cache)

					// Rewrite absolute URLs
					html = cache.absoluteURL.ReplaceAllString(html, "/"+domain+"$1")

					// Rewrite attribute URLs
					html = cache.attrURL.ReplaceAllStringFunc(html, func(match string) string {
						matches := cache.attrURL.FindStringSubmatch(match)
						if len(matches) == 3 {
							attr := matches[1]
							url := matches[2]
							if strings.HasPrefix(url, "/") && !strings.HasPrefix(url, "//") {
								return attr + `="/` + domain + url + `"`
							}
						}
						return match
					})

					// Rewrite CSS URLs
					html = cache.cssURL.ReplaceAllStringFunc(html, func(match string) string {
						matches := cache.cssURL.FindStringSubmatch(match)
						if len(matches) == 2 {
							url := strings.Trim(matches[1], `'"`)
							if strings.HasPrefix(url, "/") && !strings.HasPrefix(url, "//") {
								return `url("/` + domain + url + `")`
							}
						}
						return match
					})

					resp.Body = io.NopCloser(bytes.NewBufferString(html))
					resp.Header.Set("Content-Length", string(rune(len(html))))
				}
				return nil
			},
		}
		proxy.ServeHTTP(w, r)
	})
	http.ListenAndServe(":8080", nil)
}
