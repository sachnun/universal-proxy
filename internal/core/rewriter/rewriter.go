package rewriter

import (
	"net/url"
	"strings"
)

func ToProxyURL(rawURL, domain, proxyBase string) string {
	rawURL = strings.TrimSpace(rawURL)

	if rawURL == "" ||
		strings.HasPrefix(rawURL, "data:") ||
		strings.HasPrefix(rawURL, "javascript:") ||
		strings.HasPrefix(rawURL, "mailto:") ||
		strings.HasPrefix(rawURL, "tel:") ||
		strings.HasPrefix(rawURL, "#") ||
		strings.HasPrefix(rawURL, "blob:") {
		return rawURL
	}

	if strings.HasPrefix(rawURL, "//") {
		return proxyBase + "/" + strings.TrimPrefix(rawURL, "//")
	}

	if strings.HasPrefix(rawURL, "http://") || strings.HasPrefix(rawURL, "https://") {
		parsed, err := url.Parse(rawURL)
		if err != nil {
			return rawURL
		}
		if parsed.Host == domain {
			path := parsed.Path
			if parsed.RawQuery != "" {
				path += "?" + parsed.RawQuery
			}
			if parsed.Fragment != "" {
				path += "#" + parsed.Fragment
			}
			return proxyBase + "/" + domain + path
		}
		path := parsed.Path
		if parsed.RawQuery != "" {
			path += "?" + parsed.RawQuery
		}
		return proxyBase + "/" + parsed.Host + path
	}

	if strings.HasPrefix(rawURL, "/") {
		if strings.HasPrefix(rawURL, proxyBase+"/"+domain) ||
			strings.HasPrefix(rawURL, "/"+domain+"/") {
			return rawURL
		}
		return proxyBase + "/" + domain + rawURL
	}

	return rawURL
}
