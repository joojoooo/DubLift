package dublift

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

const originRateRetries = 5
const originRateRetryBudget = 40 * time.Second

var errOriginRateLimited = errors.New("provider rate limit persisted after bounded retries; try playback again later")

type replayReadCloser struct {
	io.Reader
	io.Closer
}

func originRateLimited(resp *http.Response, cancel context.CancelFunc) bool {
	if resp.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if resp.StatusCode != http.StatusForbidden {
		return false
	}
	// Inspect only a small error body, with its own deadline. Restore the
	// bytes so ordinary permission failures retain their response semantics.
	timer := time.AfterFunc(2*time.Second, cancel)
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	timer.Stop()
	resp.Body = &replayReadCloser{io.MultiReader(bytes.NewReader(b), resp.Body), resp.Body}
	if err != nil {
		return false
	}
	var body struct {
		Error struct {
			Errors  []struct{ Reason string }
			Details []struct{ Reason string }
		}
	}
	if json.Unmarshal(b, &body) != nil {
		return false
	}
	for _, detail := range append(body.Error.Errors, body.Error.Details...) {
		switch detail.Reason {
		case "rateLimitExceeded", "userRateLimitExceeded", "RATE_LIMIT_EXCEEDED":
			return true
		}
	}
	return false
}

func originRateRetryDelay(retryAfter string, retry int) time.Duration {
	// A little jitter prevents video and background audio retries from
	// repeatedly reaching a shared provider at the same instant.
	delay := time.Second<<min(retry, originRateRetries-1) + time.Duration(rand.Int64N(int64(time.Second)))
	if seconds, err := strconv.ParseInt(retryAfter, 10, 64); err == nil && seconds > 0 {
		// Values beyond our budget must fail without overflowing a duration or
		// retrying earlier than the provider requested.
		if seconds > int64(originRateRetryBudget/time.Second) {
			return originRateRetryBudget + time.Second
		}
		delay = max(delay, time.Duration(seconds)*time.Second)
	} else if at, err := http.ParseTime(retryAfter); err == nil {
		delay = max(delay, time.Until(at))
	}
	return delay
}

func waitOriginRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}
