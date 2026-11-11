package gmaps

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/gosom/scrapemate"
	"github.com/mxschmitt/playwright-go"
)

// cookiesFileEnv names a file with the cookies of a signed-in Google account.
// Google answers listugcposts with HTTP 403 for anonymous sessions after the
// first few reviews; with these cookies in the browser context the in-page
// review requests are authenticated and pagination reaches every review.
//
// Use a dedicated account, never a personal or business one. The file may be
// a Netscape cookies.txt or a JSON array as exported by common browser
// extensions. Cookie values are never logged.
const cookiesFileEnv = "GMAPS_COOKIES_FILE"

var (
	googleCookiesOnce sync.Once
	googleCookies     []playwright.OptionalCookie
)

// loadedGoogleCookies reads GMAPS_COOKIES_FILE once per process.
func loadedGoogleCookies() []playwright.OptionalCookie {
	googleCookiesOnce.Do(func() {
		path := strings.TrimSpace(os.Getenv(cookiesFileEnv))
		if path == "" {
			return
		}

		data, err := os.ReadFile(path) //nolint:gosec // path is the operator's own GMAPS_COOKIES_FILE setting
		if err != nil {
			log.Printf("google cookies: cannot read %s: %v", cookiesFileEnv, err)

			return
		}

		cookies, err := parseGoogleCookies(data)
		if err != nil {
			log.Printf("google cookies: %v", err)

			return
		}

		googleCookies = cookies

		log.Printf("google cookies: %d cookies loaded (signed-in session %v)", len(cookies), hasSessionCookie(cookies))
	})

	return googleCookies
}

// applyGoogleCookies adds the configured cookies to the browser context of
// page. It is a no-op without a cookie file or with a non-Playwright page.
func applyGoogleCookies(page scrapemate.BrowserPage) error {
	cookies := loadedGoogleCookies()
	if len(cookies) == 0 || page == nil {
		return nil
	}

	pwPage, ok := page.Unwrap().(playwright.Page)
	if !ok {
		return nil
	}

	if err := pwPage.Context().AddCookies(cookies); err != nil {
		return fmt.Errorf("add google cookies: %w", err)
	}

	return nil
}

// parseGoogleCookies accepts a JSON array or a Netscape cookies.txt and keeps
// only cookies for Google domains.
func parseGoogleCookies(data []byte) ([]playwright.OptionalCookie, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("cookie file is empty")
	}

	var (
		cookies []playwright.OptionalCookie
		err     error
	)

	if trimmed[0] == '[' || trimmed[0] == '{' {
		cookies, err = parseJSONCookies(trimmed)
	} else {
		cookies, err = parseNetscapeCookies(trimmed)
	}

	if err != nil {
		return nil, err
	}

	if len(cookies) == 0 {
		return nil, fmt.Errorf("cookie file has no google.com cookies")
	}

	return cookies, nil
}

type jsonCookie struct {
	Name           string   `json:"name"`
	Value          string   `json:"value"`
	Domain         string   `json:"domain"`
	Path           string   `json:"path"`
	Secure         bool     `json:"secure"`
	HTTPOnly       bool     `json:"httpOnly"`
	SameSite       string   `json:"sameSite"`
	Expires        *float64 `json:"expires"`
	ExpirationDate *float64 `json:"expirationDate"`
}

func parseJSONCookies(data []byte) ([]playwright.OptionalCookie, error) {
	var list []jsonCookie

	if data[0] == '{' {
		// Playwright storageState: {"cookies": [...]}.
		var state struct {
			Cookies []jsonCookie `json:"cookies"`
		}

		if err := json.Unmarshal(data, &state); err != nil {
			return nil, fmt.Errorf("cookie file is not valid JSON: %w", err)
		}

		list = state.Cookies
	} else if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("cookie file is not valid JSON: %w", err)
	}

	ans := make([]playwright.OptionalCookie, 0, len(list))

	for i := range list {
		c := &list[i]

		expires := c.Expires
		if expires == nil {
			expires = c.ExpirationDate
		}

		if cookie, ok := newGoogleCookie(c.Name, c.Value, c.Domain, c.Path, c.SameSite, c.Secure, c.HTTPOnly, expires); ok {
			ans = append(ans, cookie)
		}
	}

	return ans, nil
}

// parseNetscapeCookies reads the cookies.txt format: tab separated
// domain, include-subdomains, path, secure, expiry, name, value.
func parseNetscapeCookies(data []byte) ([]playwright.OptionalCookie, error) {
	var ans []playwright.OptionalCookie

	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		httpOnly := false

		if strings.HasPrefix(line, "#HttpOnly_") {
			httpOnly = true
			line = strings.TrimPrefix(line, "#HttpOnly_")
		}

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Split(line, "\t")
		if len(fields) < 7 {
			continue
		}

		var expires *float64

		if v, err := strconv.ParseFloat(fields[4], 64); err == nil && v > 0 {
			expires = &v
		}

		secure := strings.EqualFold(fields[3], "TRUE")

		if cookie, ok := newGoogleCookie(fields[5], fields[6], fields[0], fields[2], "", secure, httpOnly, expires); ok {
			ans = append(ans, cookie)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read cookies.txt: %w", err)
	}

	return ans, nil
}

func newGoogleCookie(name, value, domain, path, sameSite string, secure, httpOnly bool, expires *float64) (playwright.OptionalCookie, bool) {
	domain = strings.TrimSpace(domain)
	if name == "" || !isGoogleDomain(domain) {
		return playwright.OptionalCookie{}, false
	}

	if path == "" {
		path = "/"
	}

	cookie := playwright.OptionalCookie{
		Name:     name,
		Value:    value,
		Domain:   playwright.String(domain),
		Path:     playwright.String(path),
		Secure:   playwright.Bool(secure),
		HttpOnly: playwright.Bool(httpOnly),
	}

	// Session cookies (no expiry) are left without Expires.
	if expires != nil && *expires > 0 {
		cookie.Expires = playwright.Float(*expires)
	}

	switch strings.ToLower(sameSite) {
	case "strict":
		cookie.SameSite = playwright.SameSiteAttributeStrict
	case "lax":
		cookie.SameSite = playwright.SameSiteAttributeLax
	case "none", "no_restriction":
		cookie.SameSite = playwright.SameSiteAttributeNone
	}

	// Chromium rejects SameSite=None without Secure.
	if cookie.SameSite == playwright.SameSiteAttributeNone && !secure {
		cookie.SameSite = nil
	}

	return cookie, true
}

func isGoogleDomain(domain string) bool {
	d := strings.ToLower(strings.TrimPrefix(domain, "."))

	return d == "google.com" || strings.HasSuffix(d, ".google.com")
}

func hasSessionCookie(cookies []playwright.OptionalCookie) bool {
	for i := range cookies {
		switch cookies[i].Name {
		case "__Secure-1PSID", "SID", "__Secure-3PSID":
			return true
		}
	}

	return false
}
