package retryhttp

import (
	"net/http"

	"golang.org/x/time/rate"
)

var _ Client = (*RLClient)(nil)

type RLClient struct {
	limiter *rate.Limiter
	inner   Client
}

func NewRLClient(inner Client, limit rate.Limit, burst int) *RLClient {
	return &RLClient{
		inner:   inner,
		limiter: rate.NewLimiter(limit, burst),
	}
}

func (c *RLClient) Do(req *http.Request) (*http.Response, error) {
	if err := c.limiter.Wait(req.Context()); err != nil {
		return nil, err
	}

	return c.inner.Do(req)
}

func (c *RLClient) CloseIdleConnections() {
	c.inner.CloseIdleConnections()
}
