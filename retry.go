package retryhttp

import (
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

var _ Client = (*RetryClient)(nil)

var (
	defaultRetryWaitMin     = 1 * time.Second
	defaultRetryWaitMax     = 30 * time.Second
	defaultRetryMax         = 4
	defaultRetryAfterHeader = "Retry-After"

	redirectsErrorRe  = regexp.MustCompile(`stopped after \d+ redirects\z`)
	schemeErrorRe     = regexp.MustCompile(`unsupported protocol scheme`)
	notTrustedErrorRe = regexp.MustCompile(`certificate is not trusted`)
)

func WithInnerClient(inner Client) Option {
	return func(client *RetryClient) {
		client.inner = inner
	}
}

func WithRetryWaitMin(m time.Duration) Option {
	return func(client *RetryClient) {
		client.retryWaitMin = m
	}
}

func WithRetryWaitMax(m time.Duration) Option {
	return func(client *RetryClient) {
		client.retryWaitMax = m
	}
}

func WithRetryMax(m int) Option {
	return func(client *RetryClient) {
		client.retryMax = m
	}
}

func WithCheckRetry(checkRetry CheckRetry) Option {
	return func(client *RetryClient) {
		client.checkRetry = checkRetry
	}
}

func WithBackoff(backoff Backoff) Option {
	return func(client *RetryClient) {
		client.backoff = backoff
	}
}

func WithLogger(logger Logger) Option {
	return func(client *RetryClient) {
		client.logger = logger
	}
}

type (
	Option     func(client *RetryClient)
	CheckRetry func(ctx context.Context, resp *http.Response, err error) (bool, error)
	Backoff    func(min, max time.Duration, attemptNum int, r *http.Response) time.Duration
)

type RetryClient struct {
	backoff      Backoff
	checkRetry   CheckRetry
	inner        Client
	logger       Logger
	retryWaitMin time.Duration
	retryWaitMax time.Duration
	retryMax     int
}

func NewRetryClient(options ...Option) *RetryClient {
	c := &RetryClient{
		backoff:      DefaultBackoff(defaultRetryAfterHeader),
		checkRetry:   DefaultRetryPolicy,
		inner:        http.DefaultClient,
		logger:       noopLogger{},
		retryWaitMin: defaultRetryWaitMin,
		retryWaitMax: defaultRetryWaitMax,
		retryMax:     defaultRetryMax,
	}

	for _, option := range options {
		option(c)
	}

	return c
}

func (c *RetryClient) Do(req *http.Request) (*http.Response, error) {
	c.logger.Debug("performing request", "method", req.Method, "url", req.URL)

	var (
		r           *http.Response
		shouldRetry bool
		checkErr    error
		err         error
		attempt     int
		i           int
	)

	for attempt = 1; ; i, attempt = i+1, attempt+1 {
		r, err = c.inner.Do(req)

		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}

		if err != nil {
			c.logger.Error("request failed", "error", err, "method", req.Method, "url", req.URL)
		}

		shouldRetry, checkErr = c.checkRetry(req.Context(), r, err)

		if !shouldRetry {
			break
		}

		remain := c.retryMax - i
		if remain <= 0 {
			break
		}

		if r != nil {
			c.drainBody(r.Body)
		}

		wait := c.backoff(c.retryWaitMin, c.retryWaitMax, i, r)

		var desc string
		if r == nil {
			desc = fmt.Sprintf("%s %s", req.Method, req.URL)
		} else {
			desc = fmt.Sprintf("%s (status: %d)", desc, r.StatusCode)
		}

		c.logger.Debug("retrying request", "request", desc, "timeout", wait, "remaining", remain)

		timer := time.NewTimer(wait)
		select {
		case <-req.Context().Done():
			timer.Stop()
			c.CloseIdleConnections()
			return nil, req.Context().Err()
		case <-timer.C:
		}

		httpReq := *req
		req = &httpReq
	}

	if err == nil && checkErr == nil && !shouldRetry {
		return r, nil
	}

	defer c.CloseIdleConnections()

	if checkErr != nil {
		err = checkErr
	}

	if r != nil {
		c.drainBody(r.Body)
	}

	if err == nil {
		return nil, fmt.Errorf("%s %s giving up after %d attempt(s)", req.Method, req.URL, attempt)
	}

	return nil, fmt.Errorf("%s %s giving up after %d attempt(s): %w", req.Method, req.URL, attempt, err)
}

func (c *RetryClient) CloseIdleConnections() {
	c.inner.CloseIdleConnections()
}

func (c *RetryClient) drainBody(body io.ReadCloser) {
	defer body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(body, int64(4096)))
}

func ManyRequests(r *http.Response, header string) (time.Duration, bool) {
	if r == nil || header == "" {
		return 0, false
	}

	switch r.StatusCode {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		if s, ok := r.Header[header]; ok {
			if sleep, err := strconv.ParseInt(s[0], 10, 64); err == nil {
				return time.Second * time.Duration(sleep), true
			}
		}
	}

	return 0, false
}

func DefaultBackoff(header string) Backoff {
	return func(min, max time.Duration, attemptNum int, r *http.Response) time.Duration {
		if sleep, ok := ManyRequests(r, header); ok {
			return sleep
		}

		mult := math.Pow(2, float64(attemptNum)) * float64(min)
		sleep := time.Duration(mult)
		if float64(sleep) != mult || sleep > max {
			sleep = max
		}

		return sleep
	}
}

func DefaultRetryPolicy(ctx context.Context, resp *http.Response, err error) (bool, error) {
	// do not retry on context.Canceled or context.DeadlineExceeded
	if ctx.Err() != nil {
		return false, ctx.Err()
	}

	// don't propagate other errors
	shouldRetry, _ := baseRetryPolicy(resp, err)
	return shouldRetry, nil
}

// ErrorPropagatedRetryPolicy is the same as DefaultRetryPolicy, except it
// propagates errors back instead of returning nil. This allows you to inspect
// why it decided to retry or not.
func ErrorPropagatedRetryPolicy(ctx context.Context, resp *http.Response, err error) (bool, error) {
	// do not retry on context.Canceled or context.DeadlineExceeded
	if ctx.Err() != nil {
		return false, ctx.Err()
	}

	return baseRetryPolicy(resp, err)
}

func baseRetryPolicy(resp *http.Response, err error) (bool, error) {
	if err != nil {
		if v, ok := err.(*url.Error); ok {
			// Don't retry if the error was due to too many redirects.
			if redirectsErrorRe.MatchString(v.Error()) {
				return false, v
			}

			// Don't retry if the error was due to an invalid protocol scheme.
			if schemeErrorRe.MatchString(v.Error()) {
				return false, v
			}

			// Don't retry if the error was due to TLS cert verification failure.
			if notTrustedErrorRe.MatchString(v.Error()) {
				return false, v
			}
			if _, ok := v.Err.(x509.UnknownAuthorityError); ok {
				return false, v
			}
		}

		// The error is likely recoverable so retry.
		return true, nil
	}

	// 429 Too Many Requests is recoverable. Sometimes the server puts
	// a Retry-After response header to indicate when the server is
	// available to start processing request from client.
	if resp.StatusCode == http.StatusTooManyRequests {
		return true, nil
	}

	// Check the response code. We retry on 500-range responses to allow
	// the server time to recover, as 500's are typically not permanent
	// errors and may relate to outages on the server side. This will catch
	// invalid response codes as well, like 0 and 999.
	if resp.StatusCode == 0 || (resp.StatusCode >= 500 && resp.StatusCode != http.StatusNotImplemented) {
		return true, fmt.Errorf("unexpected HTTP status %s", resp.Status)
	}

	return false, nil
}
