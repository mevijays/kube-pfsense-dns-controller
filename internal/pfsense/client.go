// SPDX-License-Identifier: Apache-2.0

// Package pfsense is a small client for the pfSense REST API package (v2),
// limited to DNS Resolver (Unbound) host overrides.
package pfsense

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const basePath = "/api/v2/services/dns_resolver"

// HostOverride is one DNS Resolver host override.
type HostOverride struct {
	ID     int      `json:"id"`
	Host   string   `json:"host"`
	Domain string   `json:"domain"`
	IP     []string `json:"ip"`
	Descr  string   `json:"descr"`
}

// FQDN returns host.domain (or just the host when there is no domain).
func (h HostOverride) FQDN() string {
	if h.Domain == "" {
		return strings.ToLower(h.Host)
	}
	return strings.ToLower(h.Host + "." + h.Domain)
}

// Backend is what the sync loop needs from pfSense. It is an interface so the
// loop can be tested and run in dry-run mode without a firewall.
type Backend interface {
	ListHostOverrides(ctx context.Context) ([]HostOverride, error)
	// GetHostOverride returns the override currently stored at id. IDs are list
	// positions, so callers use it to check an entry is still the one they planned
	// against before changing or deleting it.
	GetHostOverride(ctx context.Context, id int) (HostOverride, error)
	CreateHostOverride(ctx context.Context, h HostOverride) error
	UpdateHostOverride(ctx context.Context, h HostOverride) error
	DeleteHostOverride(ctx context.Context, id int) error
	// Apply makes pfSense reload the resolver so pending changes take effect.
	Apply(ctx context.Context) error
}

// Client talks to the pfSense REST API.
type Client struct {
	baseURL string
	key     KeySource
	hc      *http.Client
}

// KeySource returns the API key to send with a request.
type KeySource func() (string, error)

// StaticKey always returns key.
func StaticKey(key string) KeySource {
	key = strings.TrimSpace(key) // a trailing newline is a common mistake
	return func() (string, error) { return key, nil }
}

// FileKey reads the key from path on every request, so a rotated Kubernetes
// Secret mounted as a volume is picked up without restarting the pod.
func FileKey(path string) KeySource {
	return func() (string, error) {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read API key file: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
}

// Options configure the HTTP transport.
type Options struct {
	InsecureSkipVerify bool
	CAFile             string
	Timeout            time.Duration
}

// New builds a Client. baseURL is e.g. https://pfsense.mylab.lan.
func New(baseURL string, key KeySource, opts Options) (*Client, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("pfsense url is empty")
	}
	if key == nil {
		return nil, fmt.Errorf("pfsense api key source is nil")
	}
	if k, err := key(); err != nil {
		return nil, err
	} else if k == "" {
		return nil, fmt.Errorf("pfsense api key is empty")
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: opts.InsecureSkipVerify} //nolint:gosec // opt-in for lab firewalls with self-signed certs
	if opts.CAFile != "" {
		pem, err := os.ReadFile(opts.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates found in %s", opts.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	// Start from the default transport to keep proxy-from-environment, dial and
	// TLS handshake timeouts and HTTP/2; only the TLS settings differ.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		key:     key,
		hc:      &http.Client{Timeout: timeout, Transport: tr},
	}, nil
}

type envelope struct {
	Code       int             `json:"code"`
	Status     string          `json:"status"`
	ResponseID string          `json:"response_id"`
	Message    string          `json:"message"`
	Data       json.RawMessage `json:"data"`
}

// Error is returned for any non-2xx answer from the API.
type Error struct {
	Method, Path string
	HTTPStatus   int
	Message      string
	ResponseID   string
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("pfsense %s %s: HTTP %d: %s (%s)", e.Method, e.Path, e.HTTPStatus, e.Message, e.ResponseID)
	if e.HTTPStatus == http.StatusUnauthorized || e.HTTPStatus == http.StatusForbidden {
		msg += " - check PFSENSE_API_KEY (it must be the bare key, and its user needs DNS Resolver privileges)"
	}
	return msg
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return err
	}
	key, err := c.key()
	if err != nil {
		return err
	}
	if key == "" {
		return fmt.Errorf("pfsense api key is empty")
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("pfsense %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	var env envelope
	envErr := json.Unmarshal(raw, &env) // error bodies are enveloped too
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := env.Message
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
			if len(msg) > 200 {
				msg = msg[:200]
			}
		}
		return &Error{Method: method, Path: path, HTTPStatus: resp.StatusCode, Message: msg, ResponseID: env.ResponseID}
	}
	// A 2xx answer that is not the REST API envelope (an HTML login page, a proxy,
	// a wrong base URL) must not be mistaken for success or for an empty list.
	if envErr != nil || env.Code == 0 {
		snippet := strings.TrimSpace(string(raw))
		if len(snippet) > 80 {
			snippet = snippet[:80]
		}
		return fmt.Errorf("pfsense %s %s: HTTP %d but the body is not a pfSense REST API response (check PFSENSE_URL): %q",
			method, path, resp.StatusCode, snippet)
	}
	if out != nil {
		if len(env.Data) == 0 {
			return fmt.Errorf("pfsense %s %s: response has no data", method, path)
		}
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("pfsense %s %s: decode data: %w", method, path, err)
		}
	}
	return nil
}

// ListHostOverrides returns every host override.
func (c *Client) ListHostOverrides(ctx context.Context) ([]HostOverride, error) {
	var out []HostOverride
	q := url.Values{"limit": {"0"}}
	if err := c.do(ctx, http.MethodGet, basePath+"/host_overrides", q, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetHostOverride returns the override currently stored at id.
func (c *Client) GetHostOverride(ctx context.Context, id int) (HostOverride, error) {
	var out HostOverride
	q := url.Values{"id": {fmt.Sprint(id)}}
	if err := c.do(ctx, http.MethodGet, basePath+"/host_override", q, nil, &out); err != nil {
		return HostOverride{}, err
	}
	return out, nil
}

type writeBody struct {
	ID     *int     `json:"id,omitempty"`
	Host   string   `json:"host"`
	Domain string   `json:"domain"`
	IP     []string `json:"ip"`
	Descr  string   `json:"descr"`
}

// CreateHostOverride adds a host override. The change is not live until Apply.
func (c *Client) CreateHostOverride(ctx context.Context, h HostOverride) error {
	b := writeBody{Host: h.Host, Domain: h.Domain, IP: h.IP, Descr: h.Descr}
	return c.do(ctx, http.MethodPost, basePath+"/host_override", nil, b, nil)
}

// UpdateHostOverride replaces the override with the given ID.
func (c *Client) UpdateHostOverride(ctx context.Context, h HostOverride) error {
	id := h.ID
	b := writeBody{ID: &id, Host: h.Host, Domain: h.Domain, IP: h.IP, Descr: h.Descr}
	return c.do(ctx, http.MethodPatch, basePath+"/host_override", nil, b, nil)
}

// DeleteHostOverride removes the override with the given ID.
func (c *Client) DeleteHostOverride(ctx context.Context, id int) error {
	q := url.Values{"id": {fmt.Sprint(id)}}
	return c.do(ctx, http.MethodDelete, basePath+"/host_override", q, nil, nil)
}

// Apply reloads the DNS resolver.
func (c *Client) Apply(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, basePath+"/apply", nil, map[string]any{}, nil)
}

var _ Backend = (*Client)(nil)
