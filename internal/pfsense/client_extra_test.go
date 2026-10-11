// SPDX-License-Identifier: Apache-2.0

package pfsense

import (
	"context"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

func TestKeyIsTrimmed(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("x-api-key")
		io.WriteString(w, `{"code":200,"data":[]}`)
	}))
	defer srv.Close()
	c, err := New(srv.URL, "  abc123\n", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListHostOverrides(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got != "abc123" {
		t.Fatalf("header = %q", got)
	}
}

func TestAuthFailureGivesHint(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"code":401,"status":"unauthorized","response_id":"AUTHENTICATION_FAILED","message":"Authentication failed"}`)
	})
	_, err := c.ListHostOverrides(context.Background())
	var pe *Error
	if !errors.As(err, &pe) || pe.HTTPStatus != 401 {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "PFSENSE_API_KEY") {
		t.Errorf("no hint in %q", err)
	}
}

func TestServerErrorsAndGarbage(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		want   string
	}{
		"500 html":        {500, "<html>Internal Server Error</html>", "HTTP 500"},
		"502 empty":       {502, "", "HTTP 502"},
		"200 not json":    {200, "<html>login</html>", ""},
		"200 data string": {200, `{"code":200,"data":"oops"}`, "decode data"},
		"404":             {404, `{"code":404,"message":"no such endpoint"}`, "no such endpoint"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			list, err := c.ListHostOverrides(context.Background())
			if tc.want == "" { // an HTML 200 has no envelope: treated as an empty list, never a panic
				if err != nil || len(list) != 0 {
					t.Fatalf("list=%v err=%v", list, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestContextCancelAndTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	c, _ := New(srv.URL, "k", Options{Timeout: 100 * time.Millisecond})
	start := time.Now()
	if _, err := c.ListHostOverrides(context.Background()); err == nil {
		t.Fatal("expected a timeout error")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("client timeout not honoured: %v", time.Since(start))
	}

	c, _ = New(srv.URL, "k", Options{Timeout: 5 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.Apply(ctx); err == nil {
		t.Fatal("expected context error")
	}
}

func TestUnreachableHost(t *testing.T) {
	c, _ := New("http://127.0.0.1:1", "k", Options{Timeout: time.Second})
	if _, err := c.ListHostOverrides(context.Background()); err == nil {
		t.Fatal("expected connection error")
	}
}

func TestTLSVerificationModes(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":200,"data":[]}`)
	}))
	defer srv.Close()
	ctx := context.Background()

	// default: self-signed certificate must be rejected
	c, _ := New(srv.URL, "k", Options{})
	if _, err := c.ListHostOverrides(ctx); err == nil {
		t.Fatal("self-signed certificate accepted without a CA or the insecure flag")
	}
	// opt-in insecure
	c, _ = New(srv.URL, "k", Options{InsecureSkipVerify: true})
	if _, err := c.ListHostOverrides(ctx); err != nil {
		t.Fatalf("insecure mode failed: %v", err)
	}
	// trusting the server's CA works
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caFile, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := New(srv.URL, "k", Options{CAFile: caFile})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListHostOverrides(ctx); err != nil {
		t.Fatalf("CA file mode failed: %v", err)
	}
	// bad CA file inputs are reported at construction
	bad := filepath.Join(t.TempDir(), "bad.pem")
	_ = os.WriteFile(bad, []byte("not a cert"), 0o600)
	if _, err := New(srv.URL, "k", Options{CAFile: bad}); err == nil {
		t.Error("garbage CA file accepted")
	}
	if _, err := New(srv.URL, "k", Options{CAFile: "/does/not/exist"}); err == nil {
		t.Error("missing CA file accepted")
	}
}

func TestRealWorldListShape(t *testing.T) {
	// shape captured from a real pfSense: aliases is null, ip is a list, descr may be empty.
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":200,"status":"ok","response_id":"SUCCESS","message":"","data":[
		 {"id":0,"host":"aichat","domain":"mylab.lan","ip":["172.16.1.10"],"descr":"","aliases":null},
		 {"id":1,"host":"multi","domain":"mylab.lan","ip":["10.0.0.1","10.0.0.2"],"descr":"x","aliases":[{"host":"m2","domain":"mylab.lan","descr":""}]}]}`)
	})
	got, err := c.ListHostOverrides(context.Background())
	if err != nil || len(got) != 2 || got[1].IP[1] != "10.0.0.2" || got[0].Descr != "" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestDryRunNeverWrites(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			calls++
		}
		io.WriteString(w, `{"code":200,"data":[{"id":0,"host":"a","domain":"b","ip":["1.1.1.1"]}]}`)
	}))
	defer srv.Close()
	inner, _ := New(srv.URL, "k", Options{})
	d := &DryRun{Inner: inner, Log: logr.Discard()}
	ctx := context.Background()
	if l, err := d.ListHostOverrides(ctx); err != nil || len(l) != 1 {
		t.Fatalf("reads must pass through: %v %v", l, err)
	}
	_ = d.CreateHostOverride(ctx, HostOverride{Host: "x", Domain: "y"})
	_ = d.UpdateHostOverride(ctx, HostOverride{ID: 0})
	_ = d.DeleteHostOverride(ctx, 0)
	_ = d.Apply(ctx)
	if calls != 0 {
		t.Fatalf("dry-run issued %d write calls", calls)
	}
	// no inner: behaves like an empty pfSense
	if l, err := (&DryRun{Log: logr.Discard()}).ListHostOverrides(ctx); err != nil || len(l) != 0 {
		t.Fatalf("empty dry-run: %v %v", l, err)
	}
}
