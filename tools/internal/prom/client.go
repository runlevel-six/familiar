// SPDX-License-Identifier: Apache-2.0

// Package prom is a minimal Prometheus HTTP API client (instant and range
// queries only).
package prom

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// Client queries one Prometheus.
type Client struct {
	BaseURL string // e.g. http://kps-prometheus.monitoring.svc:9090
	HTTP    *http.Client
}

// New returns a client with sane timeouts that ignores proxy env vars.
func New(base string) *Client {
	return &Client{BaseURL: base, HTTP: &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: nil}}}
}

// Sample is one series value at one time.
type Sample struct {
	Labels map[string]string `json:"labels"`
	Value  float64           `json:"value"`
}

// Series is one series over time.
type Series struct {
	Labels map[string]string `json:"labels"`
	Points [][2]float64      `json:"points"` // [unix seconds, value]
}

type apiResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string            `json:"resultType"`
		Result     []json.RawMessage `json:"result"`
	} `json:"data"`
}

func (c *Client) get(ctx context.Context, path string, q url.Values) (*apiResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	var r apiResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("prometheus: %s", resp.Status)
	}
	if r.Status != "success" {
		return nil, fmt.Errorf("prometheus %s: %s", r.ErrorType, r.Error)
	}
	return &r, nil
}

// Query runs an instant query and returns a vector (scalars become one sample).
func (c *Client) Query(ctx context.Context, promql string) ([]Sample, error) {
	r, err := c.get(ctx, "/api/v1/query", url.Values{"query": {promql}})
	if err != nil {
		return nil, err
	}
	var out []Sample
	switch r.Data.ResultType {
	case "vector":
		for _, raw := range r.Data.Result {
			var v struct {
				Metric map[string]string `json:"metric"`
				Value  [2]any            `json:"value"`
			}
			if err := json.Unmarshal(raw, &v); err != nil {
				return nil, err
			}
			f := num(v.Value[1])
			if math.IsNaN(f) || math.IsInf(f, 0) {
				continue // JSON can't carry NaN/Inf (e.g. a 0/0 ratio)
			}
			out = append(out, Sample{Labels: v.Metric, Value: f})
		}
	case "scalar":
		var pair [2]any
		if len(r.Data.Result) == 2 {
			_ = json.Unmarshal(r.Data.Result[1], &pair[1])
			out = append(out, Sample{Labels: map[string]string{}, Value: num(pair[1])})
		}
	default:
		return nil, fmt.Errorf("unexpected result type %q", r.Data.ResultType)
	}
	sort.SliceStable(out, func(i, j int) bool { return fmt.Sprint(out[i].Labels) < fmt.Sprint(out[j].Labels) })
	return out, nil
}

// QueryRange runs a range query.
func (c *Client) QueryRange(ctx context.Context, promql string, start, end time.Time, step time.Duration) ([]Series, error) {
	r, err := c.get(ctx, "/api/v1/query_range", url.Values{
		"query": {promql}, "start": {strconv.FormatInt(start.Unix(), 10)},
		"end": {strconv.FormatInt(end.Unix(), 10)}, "step": {strconv.Itoa(int(step.Seconds()))},
	})
	if err != nil {
		return nil, err
	}
	var out []Series
	for _, raw := range r.Data.Result {
		var m struct {
			Metric map[string]string `json:"metric"`
			Values [][2]any          `json:"values"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		s := Series{Labels: m.Metric}
		for _, p := range m.Values {
			if f := num(p[1]); !math.IsNaN(f) && !math.IsInf(f, 0) {
				s.Points = append(s.Points, [2]float64{num(p[0]), f})
			}
		}
		out = append(out, s)
	}
	return out, nil
}

func num(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case string:
		f, _ := strconv.ParseFloat(t, 64)
		return f
	}
	return 0
}
