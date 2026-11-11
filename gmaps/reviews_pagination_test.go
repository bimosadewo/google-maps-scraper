//nolint:testpackage // we need to test unexported functions in the same package
package gmaps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testPlaceURL = "https://www.google.com/maps/place/Test/data=!4m7!3m6!1s0x89c259ab3c1ef289:0x3b67a41175949f55!8m2"

// noSleep removes pagination delays and backoff waits for the duration of a test.
func noSleep(t *testing.T) {
	t.Helper()

	orig := reviewSleep
	reviewSleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }

	t.Cleanup(func() { reviewSleep = orig })
}

// reviewPage builds a listugcposts payload carrying a next-page token and a
// raw reviews array, in the shape extractNextPageToken/extractReviews read.
func reviewPage(t *testing.T, nextToken string, reviews []any) []byte {
	t.Helper()

	var token any
	if nextToken != "" {
		token = nextToken
	}

	if reviews == nil {
		reviews = []any{}
	}

	body, err := json.Marshal([]any{nil, token, reviews})
	require.NoError(t, err)

	return append([]byte(")]}'\n"), body...)
}

// pageToken returns the page token encoded in a generated listugcposts URL.
func pageToken(u string) string {
	i := strings.Index(u, "!2s")
	if i < 0 {
		return ""
	}

	rest := u[i+3:]
	if j := strings.Index(rest, "!"); j >= 0 {
		rest = rest[:j]
	}

	return rest
}

func newTestFetcher() *fetcher {
	return &fetcher{params: fetchReviewsParams{mapURL: testPlaceURL}}
}

// A place with far more than 50 pages must be read to the end; the old code
// stopped at 50 pages (1,000 reviews).
func Test_paginate_ReadsPastFiftyPages(t *testing.T) {
	noSleep(t)

	const total = 120

	var calls atomic.Int32

	get := func(_ context.Context, u string) ([]byte, error) {
		calls.Add(1)

		n := 0
		if tok := pageToken(u); tok != "" {
			_, err := fmt.Sscanf(tok, "p%d", &n)
			require.NoError(t, err)
		}

		next := ""
		if n+1 < total {
			next = fmt.Sprintf("p%d", n+1)
		}

		return reviewPage(t, next, nil), nil
	}

	ans, err := newTestFetcher().paginate(context.Background(), "req", get)
	require.NoError(t, err)
	assert.Len(t, ans.pages, total)
	assert.Empty(t, ans.truncated)
	assert.Equal(t, int32(total), calls.Load())
}

// A throttled page is retried instead of ending the whole fetch.
func Test_paginate_RetriesTransientFailures(t *testing.T) {
	noSleep(t)

	var failures atomic.Int32

	get := func(_ context.Context, u string) ([]byte, error) {
		switch pageToken(u) {
		case "":
			return reviewPage(t, "p1", nil), nil
		case "p1":
			// Two 429s, then an HTML block page, then success.
			switch failures.Add(1) {
			case 1, 2:
				return nil, errors.New("HTTP 429")
			case 3:
				return []byte("<html>unusual traffic</html>"), nil
			}

			return reviewPage(t, "p2", nil), nil
		default:
			return reviewPage(t, "", nil), nil
		}
	}

	ans, err := newTestFetcher().paginate(context.Background(), "req", get)
	require.NoError(t, err)
	assert.Len(t, ans.pages, 3)
	assert.Empty(t, ans.truncated)
	assert.Equal(t, int32(4), failures.Load())
}

// When a page keeps failing, the pages fetched so far are kept and the
// result is marked incomplete rather than silently looking complete.
func Test_paginate_MarksPersistentFailureAsTruncated(t *testing.T) {
	noSleep(t)

	get := func(_ context.Context, u string) ([]byte, error) {
		if pageToken(u) == "" {
			return reviewPage(t, "p1", nil), nil
		}

		return nil, errors.New("HTTP 503")
	}

	ans, err := newTestFetcher().paginate(context.Background(), "req", get)
	require.NoError(t, err)
	assert.Len(t, ans.pages, 1)
	assert.Contains(t, ans.truncated, "page 2 failed after")
	assert.Contains(t, ans.truncated, "503")
}

func Test_paginate_FirstPageFailureIsAnError(t *testing.T) {
	noSleep(t)

	get := func(context.Context, string) ([]byte, error) { return nil, errors.New("HTTP 403") }

	_, err := newTestFetcher().paginate(context.Background(), "req", get)
	require.Error(t, err)
}

// A token Google repeats would otherwise loop forever.
func Test_paginate_StopsOnRepeatedToken(t *testing.T) {
	noSleep(t)

	get := func(_ context.Context, u string) ([]byte, error) {
		if pageToken(u) == "" {
			return reviewPage(t, "loop", nil), nil
		}

		return reviewPage(t, "loop", nil), nil
	}

	ans, err := newTestFetcher().paginate(context.Background(), "req", get)
	require.NoError(t, err)
	assert.Len(t, ans.pages, 2)
}

func Test_paginate_RespectsConfiguredPageLimit(t *testing.T) {
	noSleep(t)
	t.Setenv("GMAPS_MAX_REVIEW_PAGES", "3")

	get := func(_ context.Context, u string) ([]byte, error) {
		return reviewPage(t, pageToken(u)+"x", nil), nil
	}

	ans, err := newTestFetcher().paginate(context.Background(), "req", get)
	require.NoError(t, err)
	assert.Len(t, ans.pages, 3)
	assert.Contains(t, ans.truncated, "page limit 3")
}

func Test_paginate_StopsWhenContextCancelled(t *testing.T) {
	noSleep(t)

	ctx, cancel := context.WithCancel(context.Background())

	get := func(_ context.Context, u string) ([]byte, error) {
		cancel()

		return reviewPage(t, pageToken(u)+"x", nil), nil
	}

	ans, err := newTestFetcher().paginate(ctx, "req", get)
	require.NoError(t, err)
	assert.Len(t, ans.pages, 1)
	assert.Contains(t, ans.truncated, "cancelled")
}

func Test_isReviewPayload(t *testing.T) {
	assert.True(t, isReviewPayload([]byte(")]}'\n[null,\"t\",[]]")))
	assert.True(t, isReviewPayload([]byte("[null,null,[]]")))
	assert.False(t, isReviewPayload([]byte("<!DOCTYPE html><html></html>")))
	assert.False(t, isReviewPayload([]byte("")))
	assert.False(t, isReviewPayload([]byte(")]}'\n{\"error\":1}")))
}

// The same review appearing on two pages is counted once.
func Test_AddExtraReviews_DeduplicatesByReviewID(t *testing.T) {
	raw, err := os.ReadFile("testdata/review_native_with_reply.json")
	require.NoError(t, err)

	var reviewsI []any
	require.NoError(t, json.Unmarshal(raw, &reviewsI))

	page1 := reviewPage(t, "p1", reviewsI)
	page2 := reviewPage(t, "", reviewsI)

	var e Entry
	e.AddExtraReviews([][]byte{page1, page2})

	require.Len(t, e.UserReviewsExtended, 1)
	assert.NotEmpty(t, e.UserReviewsExtended[0].ReviewID)
}
