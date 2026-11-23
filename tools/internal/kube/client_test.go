// SPDX-License-Identifier: Apache-2.0

package kube

import "testing"

func TestDevProxy(t *testing.T) {
	c, err := DevProxy("http://127.0.0.1:18001/")
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != "http://127.0.0.1:18001" || c.TokenFile != "" {
		t.Errorf("got BaseURL %q TokenFile %q", c.BaseURL, c.TokenFile)
	}
	for _, bad := range []string{
		"",
		"https://127.0.0.1:18001",            // the proxy is plain HTTP; https means a real API server
		"127.0.0.1:18001",                    // no scheme
		"http://",                            // no host
		"http://127.0.0.1:18001/api/v1/pods", // a path would be prefixed to every request
	} {
		if _, err := DevProxy(bad); err == nil {
			t.Errorf("DevProxy(%q) accepted", bad)
		}
	}
}
