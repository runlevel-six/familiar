// SPDX-License-Identifier: Apache-2.0

// vuln-read is a read-only MCP tool server over the Trivy Operator's
// VulnerabilityReports (design D27). It holds a ClusterRole that can only
// get/list those reports, and returns structured fields only: vulnerability
// IDs, severities, CVSS scores, packages and versions. Advisory titles,
// descriptions and links are never returned (they are free text written by
// third parties: untrusted input, for the Phase 3 reader).
package main

import (
	"os"
	"time"

	"github.com/runlevel-six/familiar/tools/internal/kube"
	"github.com/runlevel-six/familiar/tools/internal/mcp"
	"github.com/runlevel-six/familiar/tools/internal/redact"
)

var version = "dev"

func main() {
	log := mcp.NewLogger()
	k, err := kube.InCluster()
	// Local testing only, as in cluster-read. Never set in the manifests.
	if dev := os.Getenv("KUBE_DEV_PROXY_URL"); dev != "" {
		log.Warn("using KUBE_DEV_PROXY_URL instead of the in-cluster ServiceAccount; for local testing only", "url", dev)
		k, err = kube.DevProxy(dev)
	}
	if err != nil {
		log.Error("fatal", "err", err.Error())
		os.Exit(1)
	}
	h := newHandlers(newIndex(k, time.Now, 10*time.Minute), time.Now)
	s := &mcp.Server{
		Name: "vuln-read", Version: version, Log: log, RatePerMinute: 30,
		Instructions: "Known vulnerabilities in the images running in the cluster, from the Trivy Operator's scans. Start with vulnerability_summary. Results are structured data (IDs, severities, versions), never advisory text.",
		Tools:        h.tools(),
		Redact:       redact.Options{IPs: true},
	}
	if err := mcp.Serve(s, mcp.Env("LISTEN", ":8080")); err != nil {
		log.Error("fatal", "err", err.Error())
		os.Exit(1)
	}
}
