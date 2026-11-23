// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProbe(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	d := &net.Dialer{}
	p := &prober{target: srv.Listener.Addr().String(), hosts: []string{"example.com"}, now: time.Now, dial: d.DialContext}

	// httptest's cert isn't publicly trusted: reported, not hidden
	r := p.probe(context.Background(), "example.com")
	if r.OK || r.VerifyError == "" || r.NotAfter == "" || r.Error != "" {
		t.Fatalf("untrusted cert: %+v", r)
	}
	// trusted root and a name the cert covers
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	p.roots = pool
	if r := p.probe(context.Background(), "example.com"); !r.OK || r.DaysRemaining <= 0 {
		t.Fatalf("trusted cert: %+v", r)
	}
	// the cert doesn't cover this name
	if r := p.probe(context.Background(), "other.test"); r.OK || !strings.Contains(r.VerifyError, "other.test") {
		t.Fatalf("wrong name: %+v", r)
	}
	// connection failure
	p.target = "127.0.0.1:1"
	if r := p.probe(context.Background(), "example.com"); r.OK || !strings.HasPrefix(r.Error, "connect:") {
		t.Fatalf("connect failure: %+v", r)
	}
}

func TestOnlyConfiguredHosts(t *testing.T) {
	p := &prober{target: "127.0.0.1:1", hosts: []string{"a.example.com"}, now: time.Now, dial: (&net.Dialer{}).DialContext}
	for _, tl := range p.tools() {
		if tl.Name == "probe_certificate" {
			if _, err := tl.Handler(context.Background(), json.RawMessage(`{"host":"169.254.169.254"}`)); err == nil {
				t.Fatal("unlisted host probed")
			}
		}
	}
	for _, bad := range []string{"", "10.0.0.1", "http://x.example.com", "x.example.com:22", "*.example.com", "u@x.example.com"} {
		if _, err := parseHosts(bad); err == nil {
			t.Errorf("parseHosts accepted %q", bad)
		}
	}
	if h, err := parseHosts("B.example.com, a.example.com\nb.example.com"); err != nil || strings.Join(h, ",") != "a.example.com,b.example.com" {
		t.Fatalf("parseHosts: %v %v", h, err)
	}
}
