package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"time"
)

type ingressClient struct {
	client *http.Client
}

type httpResult struct {
	response    *http.Response
	body        []byte
	clientError error
}

type ShouldRetryHttpFunc func(result *httpResult) bool

func NewIngressClient(ingressIP string) *ingressClient {
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
	}
	return &ingressClient{
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					// For all requests to use the ingress IP address since we don't necessarily have DNS setup for the cluster
					return dialer.DialContext(ctx, network, fmt.Sprintf("%s:443", ingressIP))
				},
				ForceAttemptHTTP2:   true,
				TLSHandshakeTimeout: 5 * time.Second,
				TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
			},
		},
	}
}

func (c *ingressClient) Get(addr string) (*http.Response, error) {
	return c.client.Get(addr)
}

// RetryGet retries an HTTP get until retryFn returns false or if the
// HTTP body can not be read.
func (c *ingressClient) RetryGet(url string, retryFn ShouldRetryHttpFunc) error {
	return Retry(func() (bool, error) {
		response, err := c.Get(url)
		if err == nil {
			body, err := ioutil.ReadAll(response.Body)
			if err != nil {
				return true, err
			}
			return retryFn(&httpResult{response, body, err}), err
		}
		return retryFn(&httpResult{response: response, clientError: err}), err
	})
}
