package solver

import (
	_ "embed"
	"fmt"
	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"github.com/iancoleman/orderedmap"
	fastgen "github.com/t14raptor/go-fast/generator"
	"github.com/t14raptor/go-fast/parser"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type OneshotSolver struct {
	client     tls_client.HttpClient
	targetURL  string
	scriptURL  string
	lzAlphabet string
}

type ChallengeParams struct {
	R       string
	T       string
	Sitekey string
	Path    string
}

type SolveData struct {
	R         string
	T         string
	ScriptURL string
	Cookies   []*http.Cookie
}

func NewSolver(targetURL string) (*OneshotSolver, error) {
	client, err := BuildClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create tls client: %w", err)
	}

	return &OneshotSolver{
		client:    client,
		targetURL: strings.TrimSuffix(targetURL, "/"),
	}, nil
}

func (s *OneshotSolver) ensureScriptURL() {
	if s.scriptURL == "" {
		s.scriptURL = fmt.Sprintf("%s/cdn-cgi/challenge-platform/scripts/jsd/main.js", originFromURL(s.targetURL))
	}
}

func (s *OneshotSolver) SolveFromData(data SolveData) (*SolveResult, error) {
	if data.R == "" || data.T == "" {
		return nil, fmt.Errorf("missing required challenge data (need R and T)")
	}

	params := &ChallengeParams{
		R: data.R,
		T: data.T,
	}

	if data.ScriptURL != "" {
		s.scriptURL = data.ScriptURL
	}
	s.ensureScriptURL()

	sitekey, path, lzAlphabet, err := s.loadScript()
	if err != nil {
		return nil, fmt.Errorf("failed to parse script: %w", err)
	}
	params.Sitekey = sitekey
	params.Path = path
	s.lzAlphabet = lzAlphabet

	return s.sendOneshot(params, data.Cookies)
}

type SolveResult struct {
	Success     bool
	StatusCode  int
	Body        string
	Cookies     []*http.Cookie
	Headers     http.Header
	CfClearance string
}

func (s *OneshotSolver) loadScript() (sitekey, path, lzAlphabet string, err error) {
	req, err := http.NewRequest("GET", s.scriptURL, nil)
	if err != nil {
		return "", "", "", err
	}

	req.Header = http.Header{
		"sec-ch-ua-platform":        {`"Windows"`},
		"user-agent":                {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"},
		"sec-ch-ua":                 {`"Chromium";v="148", "Google Chrome";v="148", "Not/A)Brand";v="99"`},
		"sec-ch-ua-mobile":          {"?0"},
		"upgrade-insecure-requests": {"1"},
		"accept":                    {"*/*"},
		"sec-fetch-site":            {"same-origin"},
		"sec-fetch-mode":            {"no-cors"},
		"sec-fetch-dest":            {"script"},
		"accept-encoding":           {"gzip, deflate, br, zstd"},
		"accept-language":           {"en-US,en;q=0.9"},
		http.HeaderOrderKey: {
			"sec-ch-ua",
			"sec-ch-ua-mobile",
			"sec-ch-ua-platform",
			"upgrade-insecure-requests",
			"user-agent",
			"accept",
			"sec-fetch-site",
			"sec-fetch-mode",
			"sec-fetch-user",
			"sec-fetch-dest",
			"accept-encoding",
			"accept-language",
			"cookie",
		},
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to read script body: %w", err)
	}

	script := string(body)

	deobf, result, err := deobfuscateScript(script)
	if err != nil {
		return "", "", "", fmt.Errorf("deobfuscation failed: %w", err)
	}

	script = deobf
	lzAlphabet = result.LZAlphabet

	sitekeyRe := regexp.MustCompile(`(?:window\.)?\s*_cf_chl_opt\s*=\s*\{\s*\w+:\s*['"]([^'"]+)['"]`)
	if m := sitekeyRe.FindStringSubmatch(script); len(m) > 1 {
		sitekey = m[1]
	}

	pathRe := regexp.MustCompile(`/jsd/oneshot/([^'",\)]+)`)
	if m := pathRe.FindStringSubmatch(script); len(m) > 1 {
		path = m[1]
	}

	if path == "" {
		tableRe := regexp.MustCompile(`['"]([^'"]{500,})['"]\.split\(['"],['"]`)
		if m := tableRe.FindStringSubmatch(script); len(m) > 1 {
			table := strings.Split(m[1], ",")
			for _, entry := range table {
				if strings.HasPrefix(entry, "/jsd/oneshot/") {
					path = strings.TrimPrefix(entry, "/jsd/oneshot/")
					break
				}
			}
		}
	}

	if sitekey == "" {
		return "", "", "", fmt.Errorf("could not extract sitekey from script")
	}

	if path == "" {
		return "", "", "", fmt.Errorf("could not extract oneshot path from script")
	}

	return sitekey, path, lzAlphabet, nil
}

func (s *OneshotSolver) sendOneshot(params *ChallengeParams, cookies []*http.Cookie) (*SolveResult, error) {

	ts := time.Now().Unix()

	fingerprint := generateFingerprint(s.targetURL)

	payload := orderedmap.New()
	payload.Set("t", ts)
	payload.Set("lhr", "about:blank")
	payload.Set("api", false)
	payload.Set("c", false)
	payload.Set("payload", fingerprint)

	jsonData, err := payload.MarshalJSON()
	if err != nil {
		return nil, err
	}

	lz := NewLZString(s.lzAlphabet)
	compressed := lz.CompressToBase64(string(jsonData))

	endpoint := fmt.Sprintf("%s/cdn-cgi/challenge-platform/h/%s/jsd/oneshot/%s%s",
		originFromURL(s.targetURL), params.Sitekey, params.Path, params.R)

	req, err := http.NewRequest("POST", endpoint, strings.NewReader(compressed))
	if err != nil {
		return nil, err
	}

	for _, c := range cookies {
		req.AddCookie(c)
	}

	req.Header = http.Header{
		"sec-ch-ua-platform": {`"Windows"`},
		"user-agent":         {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"},
		"sec-ch-ua":          {`"Chromium";v="148", "Google Chrome";v="148", "Not/A)Brand";v="99"`},
		"content-type":       {"text/plain;charset=UTF-8"},
		"sec-ch-ua-mobile":   {"?0"},
		"accept":             {"*/*"},
		"origin":             {originFromURL(s.targetURL)},
		"sec-fetch-site":     {"same-origin"},
		"sec-fetch-mode":     {"cors"},
		"sec-fetch-dest":     {"empty"},
		"accept-encoding":    {"gzip, deflate, br, zstd"},
		"accept-language":    {"en-US,en;q=0.9"},
		"priority":           {"u=1, i"},
		http.HeaderOrderKey: {
			"content-length",
			"sec-ch-ua-platform",
			"user-agent",
			"sec-ch-ua",
			"content-type",
			"sec-ch-ua-mobile",
			"accept",
			"origin",
			"sec-fetch-site",
			"sec-fetch-mode",
			"sec-fetch-dest",
			"accept-encoding",
			"accept-language",
			"cookie",
			"priority",
		},
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))

	result := &SolveResult{
		StatusCode: resp.StatusCode,
		Body:       string(bodyBytes),
		Cookies:    resp.Cookies(),
		Headers:    resp.Header,
		Success:    resp.StatusCode >= 200 && resp.StatusCode < 300,
	}

	for _, c := range resp.Cookies() {
		if c.Name == "cf_clearance" {
			result.CfClearance = c.Value
			result.Success = true
		}
	}

	return result, nil
}

func originFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Path = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func deobfuscateScript(src string) (string, *DeobfuscateResult, error) {
	prog, err := parser.ParseFile(src)
	if err != nil {
		return "", nil, fmt.Errorf("parse error: %w", err)
	}

	result, err := DeobfuscateCf(prog)
	if err != nil {
		return "", nil, fmt.Errorf("deobfuscation error: %w", err)
	}

	code := fastgen.Generate(prog)
	return code, result, nil
}

func BuildClient() (tls_client.HttpClient, error) {
	opts := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(30),
		tls_client.WithClientProfile(profiles.Chrome_133),
		tls_client.WithCookieJar(tls_client.NewCookieJar()),
		tls_client.WithRandomTLSExtensionOrder(),
		tls_client.WithDisableHttp3(),
	}

	return tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
}

//go:embed fingerprint_template.json
var fingerprintTemplate string

func generateFingerprint(targetURL string) *orderedmap.OrderedMap {
	origin := originFromURL(targetURL)
	host := ""
	baseURI := targetURL
	if u, err := url.Parse(targetURL); err == nil {
		host = u.Host
		if u.Path == "" || u.Path == "/" {
			baseURI = origin + "/"
		}
	}
	lastMod := time.Now().Format("01/02/2006 15:04:05")

	s := fingerprintTemplate
	s = strings.ReplaceAll(s, "__BASEURI__", jsonEscape(baseURI))
	s = strings.ReplaceAll(s, "__ORIGIN__", jsonEscape(origin))
	s = strings.ReplaceAll(s, "__HOST__", jsonEscape(host))
	s = strings.ReplaceAll(s, "__LASTMOD__", jsonEscape(lastMod))

	o := orderedmap.New()
	_ = o.UnmarshalJSON([]byte(s))
	return o
}

func jsonEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}
