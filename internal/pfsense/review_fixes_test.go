// SPDX-License-Identifier: Apache-2.0

package pfsense

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The key file is re-read on every request, so rotating the Secret needs no restart.
func TestFileKeyIsReReadEveryRequest(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("x-api-key"))
		io.WriteString(w, `{"code":200,"data":[]}`)
	}))
	defer srv.Close()
	f := filepath.Join(t.TempDir(), "api-key")
	if err := os.WriteFile(f, []byte("old-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New(srv.URL, FileKey(f), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListHostOverrides(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte("new-key"), 0o600); err != nil { // kubelet updates the mounted Secret
		t.Fatal(err)
	}
	if _, err := c.ListHostOverrides(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != "old-key" || seen[1] != "new-key" {
		t.Fatalf("keys sent = %q", seen)
	}
	if _, err := New(srv.URL, FileKey(filepath.Join(t.TempDir(), "missing")), Options{}); err == nil {
		t.Error("missing key file accepted")
	}
}

func TestTransportKeepsProxyAndDefaults(t *testing.T) {
	c, err := New("https://pfsense.example.lan", StaticKey("k"), Options{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := c.hc.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T", c.hc.Transport)
	}
	if tr.Proxy == nil {
		t.Error("HTTPS_PROXY / NO_PROXY from the environment would be ignored")
	}
	if tr.TLSHandshakeTimeout == 0 || tr.DialContext == nil {
		t.Error("default dial / TLS handshake timeouts were dropped")
	}
	if !tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("TLS options not applied")
	}
}

func TestGetHostOverride(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v2/services/dns_resolver/host_override" || r.URL.Query().Get("id") != "7" {
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
		io.WriteString(w, `{"code":200,"status":"ok","data":{"id":7,"host":"a","domain":"mylab.lan","ip":["1.2.3.4"],"descr":"d"}}`)
	})
	h, err := c.GetHostOverride(context.Background(), 7)
	if err != nil || h.ID != 7 || h.FQDN() != "a.mylab.lan" || h.Descr != "d" {
		t.Fatalf("h=%+v err=%v", h, err)
	}
}

// pfSense's REST API reads parameters from the JSON body whenever a JSON
// Content-Type is sent, ignoring the query string: "GET host_override?id=12"
// with Content-Type: application/json answers 400 "Field `id` is required"
// (observed on a real pfSense). So GET and DELETE must not send that header.
func TestNoContentTypeOnQueryOnlyRequests(t *testing.T) {
	seen := map[string]string{}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen[r.Method] = r.Header.Get("Content-Type")
		if r.Method == http.MethodGet && r.URL.Path == "/api/v2/services/dns_resolver/host_override" {
			io.WriteString(w, `{"code":200,"data":{"id":1,"host":"a","domain":"b","ip":["1.1.1.1"]}}`)
			return
		}
		io.WriteString(w, `{"code":200,"data":[]}`)
	})
	ctx := context.Background()
	if _, err := c.GetHostOverride(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteHostOverride(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateHostOverride(ctx, HostOverride{Host: "a", Domain: "b"}); err != nil {
		t.Fatal(err)
	}
	if seen["GET"] != "" || seen["DELETE"] != "" {
		t.Fatalf("Content-Type sent on GET=%q DELETE=%q; pfSense would ignore ?id=", seen["GET"], seen["DELETE"])
	}
	if seen["POST"] != "application/json" {
		t.Fatalf("POST must send JSON, got %q", seen["POST"])
	}
}
