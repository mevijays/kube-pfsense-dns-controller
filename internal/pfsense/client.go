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
	CreateHostOverride(ctx context.Context, h HostOverride) error
	UpdateHostOverride(ctx context.Context, h HostOverride) error
	DeleteHostOverride(ctx context.Context, id int) error
	// Apply makes pfSense reload the resolver so pending changes take effect.
	Apply(ctx context.Context) error
}

// Client talks to the pfSense REST API.
type Client struct {
	baseURL string
	apiKey  string
	hc      *http.Client
}

// Options configure the HTTP transport.
type Options struct {
	InsecureSkipVerify bool
	CAFile             string
	Timeout            time.Duration
}

// New builds a Client. baseURL is e.g. https://pfsense.mylab.lan.
func New(baseURL, apiKey string, opts Options) (*Client, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("pfsense url is empty")
	}
	if apiKey == "" {
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
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		hc:      &http.Client{Timeout: timeout, Transport: &http.Transport{TLSClientConfig: tlsCfg}},
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
	return fmt.Sprintf("pfsense %s %s: HTTP %d: %s (%s)", e.Method, e.Path, e.HTTPStatus, e.Message, e.ResponseID)
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
	req.Header.Set("x-api-key", c.apiKey)
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
	_ = json.Unmarshal(raw, &env) // best effort: error bodies are enveloped too
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
	if out != nil && len(env.Data) > 0 {
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
