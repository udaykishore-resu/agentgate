package httpx

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/agentgate/agentgate/internal/telemetry"
)

// ClientConfig configures an outbound HTTP client.
type ClientConfig struct {
	Timeout             time.Duration
	MaxIdleConnsPerHost int
	MaxConnsPerHost     int
	IdleConnTimeout     time.Duration
	// CABundlePath adds an enterprise or private CA, which is what makes
	// on-premises inference over an internal PKI reachable without disabling
	// verification.
	CABundlePath string
	// ClientCertPath and ClientKeyPath enable mutual TLS to an egress proxy or
	// an on-premises endpoint that requires it.
	ClientCertPath string
	ClientKeyPath  string
	// InsecureSkipVerify is honoured only in non-production and is rejected by
	// config validation elsewhere.
	InsecureSkipVerify bool
	// Proxy, when set, forces egress through an inspecting proxy regardless of
	// environment variables.
	Proxy string
}

// NewClient builds an HTTP client with connection pooling sized for model
// traffic and TLS configured for enterprise egress paths.
func NewClient(cfg ClientConfig) (*http.Client, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 120 * time.Second
	}
	if cfg.MaxIdleConnsPerHost <= 0 {
		cfg.MaxIdleConnsPerHost = 64
	}
	if cfg.IdleConnTimeout <= 0 {
		cfg.IdleConnTimeout = 90 * time.Second
	}

	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // gated by config validation
	}
	if cfg.CABundlePath != "" {
		pem, err := os.ReadFile(cfg.CABundlePath)
		if err != nil {
			return nil, fmt.Errorf("read CA bundle %s: %w", cfg.CABundlePath, err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA bundle %s contained no usable certificates", cfg.CABundlePath)
		}
		tlsCfg.RootCAs = pool
	}
	if cfg.ClientCertPath != "" && cfg.ClientKeyPath != "" {
		cert, err := tls.LoadX509KeyPair(cfg.ClientCertPath, cfg.ClientKeyPath)
		if err != nil {
			return nil, fmt.Errorf("load client certificate: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          cfg.MaxIdleConnsPerHost * 4,
		MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
		MaxConnsPerHost:       cfg.MaxConnsPerHost,
		IdleConnTimeout:       cfg.IdleConnTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// Streaming responses must not be buffered before the first byte
		// reaches the caller.
		ResponseHeaderTimeout: 0,
		TLSClientConfig:       tlsCfg,
	}
	if cfg.Proxy != "" {
		proxyURL, err := url.Parse(cfg.Proxy)
		if err != nil {
			return nil, fmt.Errorf("parse proxy %q: %w", cfg.Proxy, err)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}

	return &http.Client{
		Transport: &tracingTransport{next: transport},
		Timeout:   cfg.Timeout,
	}, nil
}

// tracingTransport injects W3C trace context on every outbound request so the
// provider call is a child of the gateway span, and the whole agent run is one
// trace across processes.
type tracingTransport struct{ next http.RoundTripper }

func (t *tracingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	telemetry.InjectHTTP(req.Context(), req.Header)
	return t.next.RoundTrip(req)
}

// Do issues a request with a per-attempt deadline derived from the remaining
// context budget, so an attempt can never outlive the caller's deadline.
func Do(ctx context.Context, c *http.Client, req *http.Request, attemptTimeout time.Duration) (*http.Response, error) {
	if attemptTimeout > 0 {
		if dl, ok := ctx.Deadline(); ok {
			if remaining := time.Until(dl); remaining < attemptTimeout {
				attemptTimeout = remaining
			}
		}
		if attemptTimeout <= 0 {
			return nil, context.DeadlineExceeded
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, attemptTimeout)
		// The cancel must outlive the response body on the streaming path, so
		// it is attached to the response rather than deferred here.
		req = req.WithContext(ctx)
		resp, err := c.Do(req)
		if err != nil {
			cancel()
			return nil, err
		}
		resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
		return resp, nil
	}
	return c.Do(req.WithContext(ctx))
}

// cancelOnClose releases the per-attempt context when the caller finishes with
// the body. Without this a streaming response would be cancelled the moment
// the calling function returned, truncating every stream.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

// Close closes the body and releases the attempt context exactly once.
func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.once.Do(c.cancel)
	return err
}
