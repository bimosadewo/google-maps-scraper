//nolint:testpackage // we need to test unexported functions in the same package
package gmaps

import (
	"testing"

	"github.com/mxschmitt/playwright-go"
	"github.com/stretchr/testify/require"
)

func TestParseGoogleCookiesNetscape(t *testing.T) {
	data := []byte("# Netscape HTTP Cookie File\n" +
		"#HttpOnly_.google.com\tTRUE\t/\tTRUE\t1893456000\t__Secure-1PSID\tabc\n" +
		".google.com\tTRUE\t/\tFALSE\t0\tNID\txyz\n" +
		".youtube.com\tTRUE\t/\tTRUE\t1893456000\tLOGIN_INFO\tskip\n" +
		"broken line\n")

	cookies, err := parseGoogleCookies(data)
	require.NoError(t, err)
	require.Len(t, cookies, 2)

	require.Equal(t, "__Secure-1PSID", cookies[0].Name)
	require.Equal(t, ".google.com", *cookies[0].Domain)
	require.True(t, *cookies[0].HttpOnly)
	require.True(t, *cookies[0].Secure)
	require.InDelta(t, 1893456000, *cookies[0].Expires, 0)

	require.Equal(t, "NID", cookies[1].Name)
	require.Nil(t, cookies[1].Expires)
	require.False(t, *cookies[1].HttpOnly)
	require.True(t, hasSessionCookie(cookies))
}

func TestParseGoogleCookiesJSONArray(t *testing.T) {
	data := []byte(`[
		{"name":"SID","value":"v1","domain":".google.com","path":"/","secure":false,"httpOnly":false,"sameSite":"no_restriction","expirationDate":1893456000.5},
		{"name":"__Secure-3PSID","value":"v2","domain":".google.com","secure":true,"httpOnly":true,"sameSite":"none"},
		{"name":"other","value":"v3","domain":"example.com"}
	]`)

	cookies, err := parseGoogleCookies(data)
	require.NoError(t, err)
	require.Len(t, cookies, 2)

	// SameSite=None without Secure is dropped so Chromium accepts the cookie.
	require.Nil(t, cookies[0].SameSite)
	require.Equal(t, "/", *cookies[1].Path)
	require.Equal(t, playwright.SameSiteAttributeNone, cookies[1].SameSite)
}

func TestParseGoogleCookiesStorageState(t *testing.T) {
	data := []byte(`{"cookies":[{"name":"SID","value":"v","domain":"accounts.google.com","path":"/","expires":-1,"sameSite":"Lax"}],"origins":[]}`)

	cookies, err := parseGoogleCookies(data)
	require.NoError(t, err)
	require.Len(t, cookies, 1)
	require.Nil(t, cookies[0].Expires)
	require.Equal(t, playwright.SameSiteAttributeLax, cookies[0].SameSite)
}

func TestParseGoogleCookiesRejectsEmptyOrForeign(t *testing.T) {
	_, err := parseGoogleCookies([]byte("  "))
	require.Error(t, err)

	_, err = parseGoogleCookies([]byte(`[{"name":"a","value":"b","domain":".notgoogle.com"}]`))
	require.Error(t, err)

	_, err = parseGoogleCookies([]byte(`[{"name":`))
	require.Error(t, err)
}

func TestIsGoogleDomain(t *testing.T) {
	require.True(t, isGoogleDomain(".google.com"))
	require.True(t, isGoogleDomain("www.google.com"))
	require.False(t, isGoogleDomain("evilgoogle.com"))
	require.False(t, isGoogleDomain("google.com.evil.net"))
}
