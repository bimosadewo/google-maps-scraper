package gmaps

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gosom/scrapemate"
	"github.com/gosom/scrapemate/adapters/fetchers/stealth"
)

type fetchReviewsParams struct {
	page        scrapemate.BrowserPage
	mapURL      string
	reviewCount int
}

type FetchReviewsResponse struct {
	pages [][]byte
	// truncated explains why pagination stopped before Google ran out of
	// pages; empty means every page Google offered was fetched.
	truncated string
}

type fetcher struct {
	httpClient scrapemate.HTTPFetcher
	params     fetchReviewsParams
}

func newReviewFetcher(params fetchReviewsParams) *fetcher {
	netClient := stealth.New("firefox", nil)
	ans := fetcher{
		params:     params,
		httpClient: netClient,
	}

	return &ans
}

func (f *fetcher) fetch(ctx context.Context) (FetchReviewsResponse, error) {
	requestIDForSession, err := generateRandomID(21)
	if err != nil {
		return FetchReviewsResponse{}, fmt.Errorf("failed to generate session request ID: %v", err)
	}

	// First, try to fetch using the browser's session (has cookies/authentication)
	if f.params.page != nil {
		ans, err := f.paginate(ctx, requestIDForSession, f.fetchPageWithBrowser)
		if err == nil && len(ans.pages) > 0 {
			return ans, nil
		}

		log.Printf("Browser-based RPC fetch failed: %v, trying HTTP", err)
	}

	// Fallback to direct HTTP (may fail due to lack of authentication)
	ans, err := f.paginate(ctx, requestIDForSession, f.fetchReviewPage)
	if err != nil {
		log.Printf("RPC fetch failed, will try DOM extraction: %v", err)
		return FetchReviewsResponse{}, err
	}

	return ans, nil
}

// reviewPageGetter downloads one listugcposts page. It is the only part of
// pagination that differs between the browser session and plain HTTP, so the
// paging, retry and stop rules live in one place (paginate) and can be tested
// without a browser.
type reviewPageGetter func(ctx context.Context, u string) ([]byte, error)

// Pagination limits. The previous implementation stopped silently after 50
// pages (1,000 reviews) and on the first failed page, which made large places
// look complete while missing most of their reviews.
const (
	reviewPageSize = 20

	// Safety net only: 2,500 pages = 50,000 reviews. Pagination normally ends
	// when Google stops returning a next-page token.
	defaultMaxReviewPages = 2500

	// A failed page is retried with exponential backoff instead of ending the
	// whole fetch: 1s, 2s, 4s, 8s, 16s (+ jitter).
	reviewPageAttempts  = 6
	reviewRetryBaseWait = time.Second
)

// Delay between successful pages, randomized within [min, max). Overridable
// in tests.
var (
	reviewPageDelayMin = 300 * time.Millisecond
	reviewPageDelayMax = 900 * time.Millisecond
	reviewSleep        = sleepContext
)

// maxReviewPages returns the page cap; GMAPS_MAX_REVIEW_PAGES overrides it
// (0 or invalid keeps the default).
func maxReviewPages() int {
	if v, err := strconv.Atoi(os.Getenv("GMAPS_MAX_REVIEW_PAGES")); err == nil && v > 0 {
		return v
	}

	return defaultMaxReviewPages
}

// paginate walks every listugcposts page until Google stops returning a
// next-page token. Each page is retried on failure; only after all attempts
// fail is the result returned as partial (with truncated set) instead of
// pretending to be complete.
func (f *fetcher) paginate(ctx context.Context, requestID string, get reviewPageGetter) (FetchReviewsResponse, error) {
	ans := FetchReviewsResponse{}

	firstURL, err := f.generateURL(f.params.mapURL, "", reviewPageSize, requestID)
	if err != nil {
		return ans, fmt.Errorf("failed to generate initial URL: %v", err)
	}

	body, err := fetchPageWithRetry(ctx, get, firstURL)
	if err != nil {
		return ans, err
	}

	ans.pages = append(ans.pages, body)

	limit := maxReviewPages()
	seen := map[string]bool{}
	nextPageToken := extractNextPageToken(body)

	for nextPageToken != "" {
		if seen[nextPageToken] {
			// Google handed back a token we already followed; continuing would loop.
			log.Printf("reviews: repeated page token after %d pages, stopping", len(ans.pages))

			break
		}

		seen[nextPageToken] = true

		if len(ans.pages) >= limit {
			ans.truncated = fmt.Sprintf("page limit %d reached", limit)
			log.Printf("reviews: %s; set GMAPS_MAX_REVIEW_PAGES to raise it", ans.truncated)

			break
		}

		if err := reviewSleep(ctx, randomDelay(reviewPageDelayMin, reviewPageDelayMax)); err != nil {
			ans.truncated = "cancelled: " + err.Error()

			break
		}

		nextURL, err := f.generateURL(f.params.mapURL, nextPageToken, reviewPageSize, requestID)
		if err != nil {
			ans.truncated = "url error: " + err.Error()
			log.Printf("reviews: %s", ans.truncated)

			break
		}

		body, err = fetchPageWithRetry(ctx, get, nextURL)
		if err != nil {
			ans.truncated = fmt.Sprintf("page %d failed after %d attempts: %v", len(ans.pages)+1, reviewPageAttempts, err)
			log.Printf("reviews: %s", ans.truncated)

			break
		}

		ans.pages = append(ans.pages, body)
		nextPageToken = extractNextPageToken(body)
	}

	return ans, nil
}

// fetchPageWithRetry retries a page with exponential backoff. A response that
// is not a parseable listugcposts payload counts as a failure, because Google
// answers throttled requests with HTML or an empty body as well as with 429.
func fetchPageWithRetry(ctx context.Context, get reviewPageGetter, u string) ([]byte, error) {
	var lastErr error

	for attempt := 0; attempt < reviewPageAttempts; attempt++ {
		if attempt > 0 {
			wait := reviewRetryBaseWait << (attempt - 1)
			if err := reviewSleep(ctx, wait+randomDelay(0, wait/2)); err != nil {
				return nil, err
			}
		}

		body, err := get(ctx, u)
		if err == nil && !isReviewPayload(body) {
			err = errors.New("response is not a review payload")
		}

		if err == nil {
			return body, nil
		}

		lastErr = err

		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}

	return nil, lastErr
}

// isReviewPayload reports whether body is a listugcposts JSON array
// (optionally prefixed with Google's anti-XSSI guard).
func isReviewPayload(body []byte) bool {
	text := strings.TrimSpace(string(body))
	text = strings.TrimPrefix(text, ")]}'")
	text = strings.TrimSpace(text)

	if !strings.HasPrefix(text, "[") {
		return false
	}

	var arr []any

	return json.Unmarshal([]byte(text), &arr) == nil
}

func randomDelay(minDelay, maxDelay time.Duration) time.Duration {
	if maxDelay <= minDelay {
		return minDelay
	}

	n, err := rand.Int(rand.Reader, big.NewInt(int64(maxDelay-minDelay)))
	if err != nil {
		return minDelay
	}

	return minDelay + time.Duration(n.Int64())
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}

	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// fetchPageWithBrowser fetches one review page inside the Playwright page, so
// the request carries the Maps session cookies.
func (f *fetcher) fetchPageWithBrowser(_ context.Context, u string) ([]byte, error) {
	encodedURL, err := json.Marshal(u)
	if err != nil {
		return nil, err
	}

	jsCode := fmt.Sprintf(`async () => {
		try {
			const response = await fetch(%s, {
				method: 'GET',
				credentials: 'include',
				headers: {
					'Accept': '*/*',
					'Accept-Language': 'en-US,en;q=0.9'
				}
			});
			if (!response.ok) {
				return { error: 'HTTP ' + response.status };
			}
			return { data: await response.text() };
		} catch (e) {
			return { error: e.message };
		}
	}`, encodedURL)

	result, err := f.params.page.Eval(jsCode)
	if err != nil {
		return nil, fmt.Errorf("browser fetch failed: %w", err)
	}

	resultMap, ok := result.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected result type: %T", result)
	}

	if errMsg, hasError := resultMap["error"]; hasError && errMsg != nil {
		return nil, fmt.Errorf("fetch error: %v", errMsg)
	}

	data, ok := resultMap["data"].(string)
	if !ok || len(data) < 10 {
		return nil, fmt.Errorf("empty response from browser fetch")
	}

	return []byte(data), nil
}

var (
	patternsOnce sync.Once
	patterns     map[string]*regexp.Regexp
)

const hexMatchPattern = `0x[0-9a-fA-F]+:0x[0-9a-fA-F]+` // Hex format place ID

// extractPlaceID extracts the place ID from various Google Maps URL formats
func extractPlaceID(mapURL string) (string, error) {
	patternsOnce.Do(func() {
		patterns = make(map[string]*regexp.Regexp)
		// Try multiple patterns for extracting place ID
		avail := []string{
			`!1s([^!]+)`,                             // Standard format: !1s0x...
			`place_id=([^&]+)`,                       // Query parameter format
			`/place/[^/]+/@[^/]+/data=!.*!1s([^!]+)`, // Full place URL
			hexMatchPattern,                          // Hex format place ID
		}

		patterns = make(map[string]*regexp.Regexp)
		for _, p := range avail {
			patterns[p] = regexp.MustCompile(p)
		}
	})

	for pattern, re := range patterns {
		match := re.FindStringSubmatch(mapURL)
		if len(match) >= 2 {
			rawPlaceID, err := url.QueryUnescape(match[1])
			if err != nil {
				rawPlaceID = match[1]
			}

			return rawPlaceID, nil
		}
		// For hex format, match[0] is the full match
		if pattern == hexMatchPattern && len(match) >= 1 {
			return match[0], nil
		}
	}

	return "", fmt.Errorf("could not extract place ID from URL: %s", mapURL)
}

func (f *fetcher) generateURL(mapURL, pageToken string, pageSize int, requestID string) (string, error) {
	rawPlaceID, err := extractPlaceID(mapURL)
	if err != nil {
		return "", err
	}

	encodedPlaceID := url.QueryEscape(rawPlaceID)
	encodedPageToken := url.QueryEscape(pageToken)

	// Updated pb components based on current Google Maps API format (Dec 2025)
	pbComponents := []string{
		fmt.Sprintf("!1m6!1s%s", encodedPlaceID),
		"!6m4!4m1!1e1!4m1!1e3",
		fmt.Sprintf("!2m2!1i%d!2s%s", pageSize, encodedPageToken),
		fmt.Sprintf("!5m2!1s%s!7e81", requestID),
		"!8m9!2b1!3b1!5b1!7b1",
		"!12m4!1b1!2b1!4m1!1e1!11m0!13m1!1e1",
	}

	// Use English language for consistent parsing
	fullURL := fmt.Sprintf(
		"https://www.google.com/maps/rpc/listugcposts?authuser=0&hl=en&pb=%s",
		strings.Join(pbComponents, ""),
	)

	return fullURL, nil
}

func (f *fetcher) fetchReviewPage(ctx context.Context, u string) ([]byte, error) {
	job := scrapemate.Job{
		Method: requestMethodGet,
		URL:    u,
	}

	resp := f.httpClient.Fetch(ctx, &job)
	if resp.Error != nil {
		return nil, fmt.Errorf("fetch error for %s: %w", u, resp.Error)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: unexpected status code: %d", u, resp.StatusCode)
	}

	return resp.Body, nil
}

func extractNextPageToken(data []byte) string {
	text := string(data)
	prefix := ")]}'\n"
	text = strings.TrimPrefix(text, prefix)

	var result []interface{}

	err := json.Unmarshal([]byte(text), &result)
	if err != nil {
		return ""
	}

	if len(result) < 2 || result[1] == nil {
		return ""
	}

	token, ok := result[1].(string)
	if !ok {
		return ""
	}

	return token
}

func generateRandomID(length int) (string, error) {
	numBytes := (length*6 + 7) / 8
	if numBytes < 16 {
		numBytes = 16
	}

	b := make([]byte, numBytes)

	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}

	encoded := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(b)
	if len(encoded) >= length {
		return encoded[:length], nil
	}

	return "", errors.New("generated ID is shorter than expected")
}

// DOMReview represents a review extracted from the DOM
type DOMReview struct {
	ReviewID                string
	AuthorName              string
	AuthorURL               string
	ProfilePicture          string
	Rating                  int
	RelativeTimeDescription string
	Text                    string
	Images                  []string
}

// ConvertDOMReviewsToReviews converts DOMReview slice to Review slice
func ConvertDOMReviewsToReviews(domReviews []DOMReview) []Review {
	reviews := make([]Review, 0, len(domReviews))

	for i := range domReviews {
		dr := &domReviews[i]
		review := Review{
			Name:           dr.AuthorName,
			ProfilePicture: dr.ProfilePicture,
			Rating:         dr.Rating,
			Description:    dr.Text,
			When:           dr.RelativeTimeDescription,
			Images:         dr.Images,
			ReviewID:       dr.ReviewID,
		}

		if review.Name != "" {
			reviews = append(reviews, review)
		}
	}

	return reviews
}

// dedupeDOMReviewsAgainstPrimary drops DOM reviews that are already present in the
// primary reviews. Matching is by review id only, an empty id is never a duplicate.
func dedupeDOMReviewsAgainstPrimary(primary, domReviews []Review) []Review {
	seen := make(map[string]struct{}, len(primary))

	for i := range primary {
		if primary[i].ReviewID != "" {
			seen[primary[i].ReviewID] = struct{}{}
		}
	}

	if len(seen) == 0 {
		return domReviews
	}

	kept := make([]Review, 0, len(domReviews))
	withID := 0

	for i := range domReviews {
		if domReviews[i].ReviewID != "" {
			withID++

			if _, dup := seen[domReviews[i].ReviewID]; dup {
				continue
			}
		}

		kept = append(kept, domReviews[i])
	}

	if withID > 0 && len(kept) == len(domReviews) {
		log.Printf("DOM review ids matched none of %d primary ids", len(seen))
	}

	return kept
}

func decodeDOMReviews(raw []any) []DOMReview {
	decoded := make([]DOMReview, 0, len(raw))

	for _, rawReview := range raw {
		reviewMap, ok := rawReview.(map[string]interface{})
		if !ok {
			continue
		}

		review := DOMReview{}
		if v, ok := reviewMap["review_id"].(string); ok {
			review.ReviewID = v
		}

		if v, ok := reviewMap["author_name"].(string); ok {
			review.AuthorName = v
		}

		if v, ok := reviewMap["author_url"].(string); ok {
			review.AuthorURL = v
		}

		if v, ok := reviewMap["profile_picture"].(string); ok {
			review.ProfilePicture = v
		}

		// Playwright decodes whole JS numbers as int, fractional ones as float64.
		switch v := reviewMap["rating"].(type) {
		case int:
			review.Rating = v
		case float64:
			review.Rating = int(v)
		}

		// The rating selectors also match non-star aria-labels, e.g. "12 reviews".
		if review.Rating < 1 || review.Rating > 5 {
			review.Rating = 0
		}

		if v, ok := reviewMap["relative_time_description"].(string); ok {
			review.RelativeTimeDescription = v
		}

		if v, ok := reviewMap["text"].(string); ok {
			review.Text = v
		}

		if v, ok := reviewMap["images"].([]interface{}); ok {
			for _, img := range v {
				if imgStr, ok := img.(string); ok {
					review.Images = append(review.Images, imgStr)
				}
			}
		}

		decoded = append(decoded, review)
	}

	return decoded
}

// extractReviewsFromPage extracts reviews directly from the page DOM
// This is a fallback when the RPC API fails
func extractReviewsFromPage(ctx context.Context, page scrapemate.BrowserPage) ([]DOMReview, error) {
	log.Printf("Attempting DOM-based review extraction")

	// First, try to click the reviews section to open the reviews panel
	clickedReviews, _ := page.Eval(`() => {
		try {
			// Method 1: Click on the reviews count/link in the place info
			const reviewsButtons = document.querySelectorAll('button[jsaction*="reviewChart"], button[jsaction*="reviews"]');
			for (const btn of reviewsButtons) {
				if (btn.textContent.includes('review') || btn.getAttribute('aria-label')?.includes('review')) {
					btn.click();
					return 'reviews_button';
				}
			}

			// Method 2: Click on the reviews tab
			const tabs = document.querySelectorAll('button[role="tab"]');
			for (const tab of tabs) {
				const label = tab.getAttribute('aria-label') || tab.textContent || '';
				if (label.toLowerCase().includes('review')) {
					tab.click();
					return 'reviews_tab';
				}
			}

			// Method 3: Click the star rating area which often opens reviews
			const ratingArea = document.querySelector('.F7nice, .fontDisplayLarge');
			if (ratingArea) {
				ratingArea.click();
				return 'rating_area';
			}

			// Method 4: Look for "See all reviews" or similar links
			const allLinks = document.querySelectorAll('a, button');
			for (const link of allLinks) {
				const text = link.textContent?.toLowerCase() || '';
				if (text.includes('all review') || text.includes('see review') || text.includes('more review')) {
					link.click();
					return 'all_reviews_link';
				}
			}

			return false;
		} catch (e) {
			console.error('Error clicking reviews:', e);
			return false;
		}
	}`)

	if clickedReviews != nil && clickedReviews != false {
		log.Printf("Clicked reviews via: %v", clickedReviews)
	}

	// Wait for reviews panel to load
	time.Sleep(3 * time.Second)

	var reviews []DOMReview

	maxScrollAttempts := 30
	lastCount := 0
	stuckCount := 0

	for attempt := 0; attempt < maxScrollAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return reviews, ctx.Err()
		default:
		}

		// Extract reviews from the DOM - updated for Dec 2025 Google Maps structure
		reviewsJSON, err := page.Eval(`() => {
			try {
				const reviews = [];

				// Try multiple selectors for review container elements
				// Google Maps uses various class names that change over time
				const reviewSelectors = [
					'.jftiEf',                           // Common review container
					'div[data-review-id]',               // Review with ID attribute
					'.gws-localreviews__google-review',  // Alternative format
					'[data-hveid] .review-dialog-list > div', // Search results reviews
					'.WMbnJf',                           // Another review container
					'.bwb7ce',                           // New review format
				];

				let reviewElements = [];
				for (const selector of reviewSelectors) {
					const elements = document.querySelectorAll(selector);
					if (elements && elements.length > 0) {
						reviewElements = Array.from(elements);
						console.log('Found reviews with selector:', selector, 'count:', elements.length);
						break;
					}
				}

				// If no reviews found with specific selectors, try to find by structure
				if (reviewElements.length === 0) {
					// Look for elements that look like reviews (have rating + text)
					const allDivs = document.querySelectorAll('div[class]');
					for (const div of allDivs) {
						const hasRating = div.querySelector('[aria-label*="star"], [role="img"][aria-label*="star"]');
						const hasText = div.querySelector('span.wiI7pd, span[class*="review"]');
						if (hasRating && hasText && !reviewElements.includes(div)) {
							reviewElements.push(div);
						}
					}
				}

				console.log('Total review elements found:', reviewElements.length);

				for (const element of reviewElements) {
					try {
						// The card carries the id, unless the selector matched a wrapper
						const reviewId = element.getAttribute('data-review-id') ||
							element.querySelector('[data-review-id]')?.getAttribute('data-review-id') || '';

						// Author name - comprehensive selectors
						const userSelectors = [
							'.d4r55',           // Primary name class
							'.WNxzHc',          // Alternative name
							'.TSUbDb a',        // Link with name
							'.review-author',   // Generic
							'button.al6Kxe',    // Clickable name
							'.bHrnEe',          // Another name container
						];
						let userName = '';
						let userUrl = '';
						for (const sel of userSelectors) {
							const el = element.querySelector(sel);
							if (el) {
								userName = el.textContent?.trim() || '';
								if (el.tagName?.toLowerCase() === 'a') {
									userUrl = el.getAttribute('href') || '';
								}
								if (userName) break;
							}
						}

						// Profile picture - multiple patterns
						const profilePicSelectors = [
							'.NBa7we',
							'img[src*="googleusercontent"]',
							'img[src*="lh3.google"]',
							'.review-author-photo img',
						];
						let profilePic = '';
						for (const sel of profilePicSelectors) {
							const el = element.querySelector(sel);
							if (el) {
								profilePic = el.getAttribute('src') || '';
								if (profilePic) break;
							}
						}

						// Rating - try multiple approaches
						let rating = 0;
						const ratingSelectors = [
							'.kvMYJc',
							'.DU9Pgb span[aria-label]',
							'[role="img"][aria-label*="star"]',
							'.pjemBf span',
							'.review-score',
						];
						for (const sel of ratingSelectors) {
							const ratingEl = element.querySelector(sel);
							if (ratingEl) {
								const ariaLabel = ratingEl.getAttribute('aria-label') || '';
								// Match patterns like "5 stars", "Rated 4 out of 5", "4.5 étoiles"
								const match = ariaLabel.match(/(\d+(?:\.\d+)?)/);
								if (match) {
									rating = Math.round(parseFloat(match[1])) || 0;
									break;
								}
								// Also try counting filled stars
								const filledStars = element.querySelectorAll('.hCCjke.vzX5Ic, [aria-label*="star"][style*="color"]').length;
								if (filledStars > 0) {
									rating = filledStars;
									break;
								}
							}
						}

						// Time/date - multiple selectors
						const timeSelectors = ['.rsqaWe', '.DU9Pgb', '.tTVLSc', '.review-date', '.dehysf'];
						let relativeTime = '';
						for (const sel of timeSelectors) {
							const el = element.querySelector(sel);
							if (el) {
								const text = el.textContent?.trim() || '';
								// Look for time-related text (ago, month, year, etc)
								if (text && (text.includes('ago') || text.includes('week') || text.includes('month') ||
								    text.includes('year') || text.includes('day') || text.match(/\d{4}/))) {
									relativeTime = text;
									break;
								}
							}
						}

						// Review text - try to expand and get full text
						const textSelectors = [
							'.wiI7pd',
							'.MyEned span',
							'.review-full-text',
							'.Jtu6Td span',
							'[data-expandable-section] span',
						];
						let text = '';

						// First try to click "More" button to expand text
						const moreButtons = element.querySelectorAll('.w8nwRe, button[aria-label*="More"], button[aria-expanded="false"]');
						for (const btn of moreButtons) {
							try { btn.click(); } catch(e) {}
						}

						for (const sel of textSelectors) {
							const textEl = element.querySelector(sel);
							if (textEl) {
								text = textEl.textContent?.trim() || '';
								if (text && text.length > 5) break;
							}
						}

						// Images
						const imageElements = element.querySelectorAll('.KtCyie img, .Tya61d img, .review-photos img, img[src*="lh3"]');
						const images = [];
						for (const img of imageElements) {
							const src = img.getAttribute('src') || '';
							if (src && !src.includes('data:image') && !src.includes('profile')) {
								images.push(src);
							}
						}

						if (userName && (text || rating > 0)) {
							reviews.push({
								review_id: reviewId,
								author_name: userName,
								author_url: userUrl,
								profile_picture: profilePic,
								rating: rating,
								relative_time_description: relativeTime,
								text: text,
								images: images
							});
						}
					} catch (e) {
						console.error("Error extracting review:", e);
					}
				}

				return reviews;
			} catch (e) {
				console.error("Error in review extraction:", e);
				return [];
			}
		}`)
		if err != nil {
			log.Printf("Error extracting reviews from DOM: %v", err)
		} else if reviewsJSON != nil {
			rawReviews, ok := reviewsJSON.([]any)
			if ok {
				decoded := decodeDOMReviews(rawReviews)

				for i := range decoded {
					// Add if unique (check by author name and text prefix)
					isDuplicate := false

					for j := range reviews {
						if reviews[j].AuthorName == decoded[i].AuthorName {
							if reviews[j].Text == decoded[i].Text {
								isDuplicate = true
								break
							}

							if len(reviews[j].Text) > 20 && len(decoded[i].Text) > 20 &&
								reviews[j].Text[:20] == decoded[i].Text[:20] {
								isDuplicate = true
								break
							}
						}
					}

					if !isDuplicate && decoded[i].AuthorName != "" {
						reviews = append(reviews, decoded[i])
					}
				}
			}
		}

		currentCount := len(reviews)
		if currentCount == lastCount {
			stuckCount++
			if stuckCount > 5 {
				log.Printf("Review count stuck at %d, stopping scroll", currentCount)
				break
			}
		} else {
			stuckCount = 0
			lastCount = currentCount
		}

		// Scroll to load more reviews
		_, _ = page.Eval(`() => {
			try {
				// Try multiple scroll containers
				const selectors = [
					'.m6QErb.DxyBCb.kA9KIf.dS8AEf',
					'.m6QErb.DxyBCb.kA9KIf',
					'.DxyBCb.kA9KIf',
					'.m6QErb',
					'.section-scrollbox',
					'div[role="feed"]'
				];

				for (const selector of selectors) {
					const el = document.querySelector(selector);
					if (el) {
						el.scrollBy(0, 800);
						return true;
					}
				}

				window.scrollBy(0, 800);
				return true;
			} catch (e) {
				window.scrollBy(0, 800);
				return false;
			}
		}`)

		time.Sleep(500 * time.Millisecond)
	}

	log.Printf("DOM extraction completed: %d reviews found", len(reviews))

	return reviews, nil
}

// FetchReviewsWithFallback attempts RPC-based extraction first, then falls back to DOM
func FetchReviewsWithFallback(ctx context.Context, params fetchReviewsParams) (FetchReviewsResponse, []DOMReview, error) {
	fetcher := newReviewFetcher(params)

	// Try RPC-based extraction first
	rpcResponse, err := fetcher.fetch(ctx)
	if err == nil && len(rpcResponse.pages) > 0 {
		// Validate that we actually got reviews
		totalReviews := 0

		for _, page := range rpcResponse.pages {
			reviews := extractReviews(page)
			totalReviews += len(reviews)
		}

		if totalReviews > 0 {
			log.Printf("RPC extraction: %d pages, %d reviews of %d listed on the place%s",
				len(rpcResponse.pages), totalReviews, params.reviewCount, truncationNote(rpcResponse.truncated))

			return rpcResponse, nil, nil
		}

		log.Printf("RPC returned empty reviews, trying DOM extraction")
	}

	// Fallback to DOM-based extraction
	if params.page != nil {
		domReviews, domErr := extractReviewsFromPage(ctx, params.page)
		if domErr == nil && len(domReviews) > 0 {
			log.Printf("DOM extraction successful: %d reviews", len(domReviews))
			return FetchReviewsResponse{}, domReviews, nil
		}

		if domErr != nil {
			log.Printf("DOM extraction failed: %v", domErr)
		}
	}

	// Return whatever we have
	if err != nil {
		return FetchReviewsResponse{}, nil, fmt.Errorf("all review extraction methods failed: %v", err)
	}

	return rpcResponse, nil, nil
}

func truncationNote(reason string) string {
	if reason == "" {
		return ""
	}

	return " (INCOMPLETE: " + reason + ")"
}
