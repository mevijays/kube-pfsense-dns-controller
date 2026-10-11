// SPDX-License-Identifier: Apache-2.0

package pfsense

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "secret-key", Options{})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestList(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v2/services/dns_resolver/host_overrides" {
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
		if r.Header.Get("x-api-key") != "secret-key" {
			t.Errorf("api key header = %q", r.Header.Get("x-api-key"))
		}
		io.WriteString(w, `{"code":200,"status":"ok","data":[{"id":0,"host":"jenkins","domain":"mylab.lan","ip":["172.16.1.61"],"descr":"managed-by=x"}]}`)
	})
	got, err := c.ListHostOverrides(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].FQDN() != "jenkins.mylab.lan" || got[0].IP[0] != "172.16.1.61" {
		t.Fatalf("got %+v", got)
	}
}

func TestWriteCalls(t *testing.T) {
	type call struct{ method, path, query, body string }
	var calls []call
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls = append(calls, call{r.Method, r.URL.Path, r.URL.RawQuery, strings.TrimSpace(string(b))})
		io.WriteString(w, `{"code":200,"status":"ok","data":{}}`)
	})
	ctx := context.Background()
	h := HostOverride{ID: 3, Host: "app1", Domain: "mylab.lan", IP: []string{"1.2.3.4"}, Descr: "managed-by=x"}
	for _, err := range []error{c.CreateHostOverride(ctx, h), c.UpdateHostOverride(ctx, h), c.DeleteHostOverride(ctx, 3), c.Apply(ctx)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	want := []call{
		{"POST", "/api/v2/services/dns_resolver/host_override", "", `{"host":"app1","domain":"mylab.lan","ip":["1.2.3.4"],"descr":"managed-by=x"}`},
		{"PATCH", "/api/v2/services/dns_resolver/host_override", "", `{"id":3,"host":"app1","domain":"mylab.lan","ip":["1.2.3.4"],"descr":"managed-by=x"}`},
		{"DELETE", "/api/v2/services/dns_resolver/host_override", "id=3", ""},
		{"POST", "/api/v2/services/dns_resolver/apply", "", `{}`},
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %+v", calls)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("call %d = %+v, want %+v", i, calls[i], want[i])
		}
	}
	// Create must not send an id.
	var m map[string]any
	_ = json.Unmarshal([]byte(calls[0].body), &m)
	if _, has := m["id"]; has {
		t.Error("create body contains id")
	}
}

func TestErrorIsReported(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, `{"code":409,"status":"conflict","response_id":"HOST_OVERRIDE_ALREADY_EXISTS","message":"already exists"}`)
	})
	err := c.CreateHostOverride(context.Background(), HostOverride{Host: "a", Domain: "b"})
	var pe *Error
	if !errors.As(err, &pe) || pe.HTTPStatus != http.StatusConflict || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v", err)
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := New("", "k", Options{}); err == nil {
		t.Error("empty url accepted")
	}
	if _, err := New("https://x", "", Options{}); err == nil {
		t.Error("empty key accepted")
	}
}
