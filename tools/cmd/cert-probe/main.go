// SPDX-License-Identifier: Apache-2.0

// cert-probe is a read-only MCP tool server that checks the TLS certificate
// each ingress host actually serves (design §4.1). It holds no credentials.
// Hosts come from a fixed allowlist (HOSTS), never from the caller, so it
// can't be used to probe arbitrary addresses (SSRF). Connections go to the
// ingress controller (TARGET) with the host as SNI, so no DNS is needed.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/runlevel-six/familiar/tools/internal/mcp"
	"github.com/runlevel-six/familiar/tools/internal/redact"
)

var version = "dev"

type prober struct {
	target string // host:port of the ingress controller
	hosts  []string
	roots  *x509.CertPool // nil = system roots
	now    func() time.Time
	dial   func(ctx context.Context, network, addr string) (net.Conn, error)
}

type result struct {
	Host          string   `json:"host"`
	OK            bool     `json:"ok"`
	Error         string   `json:"error,omitempty"`
	VerifyError   string   `json:"verify_error,omitempty"`
	Subject       string   `json:"subject,omitempty"`
	DNSNames      []string `json:"dns_names,omitempty"`
	Issuer        string   `json:"issuer,omitempty"`
	NotBefore     string   `json:"not_before,omitempty"`
	NotAfter      string   `json:"not_after,omitempty"`
	DaysRemaining float64  `json:"days_remaining,omitempty"`
	TLSVersion    string   `json:"tls_version,omitempty"`
}

func (p *prober) probe(ctx context.Context, host string) result {
	r := result{Host: host}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	raw, err := p.dial(ctx, "tcp", p.target)
	if err != nil {
		r.Error = "connect: " + err.Error()
		return r
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(8 * time.Second))
	// Handshake without verification so the served certificate can be
	// reported even when it's wrong; then verify explicitly.
	conn := tls.Client(raw, &tls.Config{ServerName: host, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) //nolint:gosec // verified below
	if err := conn.HandshakeContext(ctx); err != nil {
		r.Error = "handshake: " + err.Error()
		return r
	}
	st := conn.ConnectionState()
	if len(st.PeerCertificates) == 0 {
		r.Error = "no certificate presented"
		return r
	}
	leaf := st.PeerCertificates[0]
	inter := x509.NewCertPool()
	for _, c := range st.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	_, verr := leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: p.roots, Intermediates: inter, CurrentTime: p.now()})
	r.Subject, r.DNSNames, r.Issuer = leaf.Subject.CommonName, leaf.DNSNames, leaf.Issuer.CommonName
	r.NotBefore, r.NotAfter = leaf.NotBefore.UTC().Format(time.RFC3339), leaf.NotAfter.UTC().Format(time.RFC3339)
	r.DaysRemaining = float64(int(leaf.NotAfter.Sub(p.now()).Hours()/24*10)) / 10
	r.TLSVersion = tls.VersionName(st.Version)
	if verr != nil {
		r.VerifyError = verr.Error()
	}
	r.OK = verr == nil
	return r
}

func (p *prober) tools() []mcp.Tool {
	return []mcp.Tool{
		{
			Name: "probe_all_certificates", Title: "Probe all ingress certificates",
			Description: "Connect to the ingress controller for every configured host and report the certificate it serves: validity for that host, issuer, expiry and days remaining. Hosts: " + strings.Join(p.hosts, ", "),
			InputSchema: mcp.Object(map[string]any{}),
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
				var in struct{}
				if err := mcp.Decode(args, &in); err != nil {
					return nil, err
				}
				out := make([]result, len(p.hosts))
				var wg sync.WaitGroup
				for i, h := range p.hosts {
					wg.Go(func() { ; out[i] = p.probe(ctx, h) })
				}
				wg.Wait()
				bad := 0
				for _, r := range out {
					if !r.OK {
						bad++
					}
				}
				return map[string]any{"results": out, "problems": bad, "at": p.now().UTC().Format(time.RFC3339)}, nil
			},
		},
		{
			Name: "probe_certificate", Title: "Probe one ingress certificate",
			Description: "Probe one configured host (see probe_all_certificates for the list).",
			InputSchema: mcp.Object(map[string]any{"host": mcp.Enum("Host to probe", p.hosts...)}, "host"),
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
				var in struct {
					Host string `json:"host"`
				}
				if err := mcp.Decode(args, &in); err != nil {
					return nil, err
				}
				if !contains(p.hosts, in.Host) {
					return nil, mcp.Errorf("host %q is not in the configured list", in.Host)
				}
				return p.probe(ctx, in.Host), nil
			},
		},
	}
}

func contains(l []string, s string) bool {
	return slices.Contains(l, s)
}

func parseHosts(s string) ([]string, error) {
	var hosts []string
	for _, h := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if strings.ContainsAny(h, "/:@*") || net.ParseIP(h) != nil {
			return nil, errors.New("HOSTS must be plain DNS names: " + h)
		}
		if !contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	sort.Strings(hosts)
	if len(hosts) == 0 {
		return nil, errors.New("HOSTS is empty")
	}
	return hosts, nil
}

func main() {
	log := mcp.NewLogger()
	hosts, err := parseHosts(os.Getenv("HOSTS"))
	if err != nil {
		log.Error("fatal", "err", err.Error())
		os.Exit(1)
	}
	d := &net.Dialer{Timeout: 5 * time.Second}
	p := &prober{target: mcp.Env("TARGET", "traefik.traefik.svc:443"), hosts: hosts, now: time.Now, dial: d.DialContext}
	s := &mcp.Server{
		Name: "cert-probe", Version: version, Log: log, RatePerMinute: 20,
		Instructions: "Checks the TLS certificates served for a fixed list of ingress hosts.",
		Tools:        p.tools(),
		Redact:       redact.Options{IPs: true},
	}
	if err := mcp.Serve(s, mcp.Env("LISTEN", ":8080")); err != nil {
		log.Error("fatal", "err", err.Error())
		os.Exit(1)
	}
}
