// SPDX-License-Identifier: Apache-2.0

// Package kube is a minimal read-only Kubernetes API client (stdlib only)
// using the pod's projected ServiceAccount token. It only issues GETs.
package kube

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// ErrNotFound is returned for 404s (missing object or missing CRD).
var ErrNotFound = errors.New("not found")

// Client talks to the API server.
type Client struct {
	BaseURL   string
	TokenFile string // re-read on every request: projected tokens rotate
	HTTP      *http.Client
}

// InCluster builds a client from the pod's ServiceAccount mount.
func InCluster() (*Client, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("not running in a cluster (KUBERNETES_SERVICE_HOST unset)")
	}
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("no certificates in ca.crt")
	}
	return &Client{
		BaseURL:   "https://" + net.JoinHostPort(host, port),
		TokenFile: saDir + "/token",
		HTTP: &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{
			Proxy:           nil,
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		}},
	}, nil
}

// DevProxy builds a client for a local `kubectl proxy` (plain HTTP, no
// token: the proxy authenticates). For local testing only.
func DevProxy(base string) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" || u.Host == "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("KUBE_DEV_PROXY_URL must be http://host:port, got %q", base)
	}
	return &Client{
		BaseURL: strings.TrimSuffix(base, "/"),
		HTTP:    &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: nil}},
	}, nil
}

// Get fetches path (e.g. /api/v1/pods?limit=500) into out.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("bad path %q", path)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	if c.TokenFile != "" {
		tok, err := os.ReadFile(c.TokenFile)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode >= 300:
		return fmt.Errorf("GET %s: %s", strings.SplitN(path, "?", 2)[0], resp.Status)
	}
	return json.Unmarshal(body, out)
}
