// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Budgets tracks USD spend per role in fixed UTC hour and day windows.
// Enforcement happens before a request is forwarded (reject when the window
// is already spent); the request's own cost is added afterwards, so a window
// can overshoot by at most one request, which max_tokens_per_request bounds.
type Budgets struct {
	mu    sync.Mutex
	path  string
	State map[string]*RoleSpend `json:"roles"`
}

type RoleSpend struct {
	Hour      string  `json:"hour"` // e.g. 2026-11-22T03
	HourUSD   float64 `json:"hour_usd"`
	Day       string  `json:"day"` // e.g. 2026-11-22
	DayUSD    float64 `json:"day_usd"`
	TotalUSD  float64 `json:"total_usd"`
	UpdatedAt string  `json:"updated_at"`
}

func newBudgets(path string) (*Budgets, error) {
	b := &Budgets{path: path, State: map[string]*RoleSpend{}}
	if path == "" {
		return b, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return b, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, b); err != nil {
		return nil, err
	}
	if b.State == nil {
		b.State = map[string]*RoleSpend{}
	}
	return b, nil
}

func windows(now time.Time) (hour, day string) {
	u := now.UTC()
	return u.Format("2006-01-02T15"), u.Format("2006-01-02")
}

// rollLocked resets windows that have passed. Caller holds mu.
func (b *Budgets) rollLocked(role string, now time.Time) *RoleSpend {
	s := b.State[role]
	if s == nil {
		s = &RoleSpend{}
		b.State[role] = s
	}
	h, d := windows(now)
	if s.Hour != h {
		s.Hour, s.HourUSD = h, 0
	}
	if s.Day != d {
		s.Day, s.DayUSD = d, 0
	}
	return s
}

// Check returns a non-empty reason if the role has exhausted a window.
func (b *Budgets) Check(r *Role, now time.Time) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.rollLocked(r.Name, now)
	switch {
	case r.HourlyUSD > 0 && s.HourUSD >= r.HourlyUSD:
		return "hourly_budget_exhausted"
	case r.DailyUSD > 0 && s.DayUSD >= r.DailyUSD:
		return "daily_budget_exhausted"
	}
	return ""
}

// Add records spend and persists state. Returns the window totals.
func (b *Budgets) Add(role string, usd float64, now time.Time) (hourUSD, dayUSD float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.rollLocked(role, now)
	s.HourUSD += usd
	s.DayUSD += usd
	s.TotalUSD += usd
	s.UpdatedAt = now.UTC().Format(time.RFC3339)
	b.saveLocked()
	return s.HourUSD, s.DayUSD
}

// Snapshot returns current window totals (after rolling).
func (b *Budgets) Snapshot(role string, now time.Time) RoleSpend {
	b.mu.Lock()
	defer b.mu.Unlock()
	return *b.rollLocked(role, now)
}

func (b *Budgets) saveLocked() {
	if b.path == "" {
		return
	}
	data, err := json.Marshal(b)
	if err != nil {
		return
	}
	tmp := filepath.Join(filepath.Dir(b.path), ".budget-state.tmp")
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, b.path)
	}
}
