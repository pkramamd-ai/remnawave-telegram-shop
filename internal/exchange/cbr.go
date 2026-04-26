// Package exchange provides currency exchange rates with caching.
package exchange

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const cbrURL = "https://www.cbr-xml-daily.ru/daily_json.js"

// Provider returns RUB-based exchange rates pulled from the Russian Central
// Bank (cbr-xml-daily.ru). Results are cached in memory for the configured TTL.
//
// A zero-valued Provider is not usable; construct via [NewProvider].
type Provider struct {
	mu      sync.RWMutex
	usdRate float64
	fetched time.Time
	ttl     time.Duration
	client  *http.Client
}

// NewProvider returns a Provider with a 1-hour cache TTL and a 10-second HTTP
// timeout.
func NewProvider() *Provider {
	return &Provider{
		ttl:    1 * time.Hour,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// USDRate returns how many rubles equal one US dollar according to the CBR
// daily feed. The value is refreshed at most once per TTL; if the upstream is
// unreachable but a previous value was cached, the stale value is returned
// instead of an error.
func (p *Provider) USDRate(ctx context.Context) (float64, error) {
	p.mu.RLock()
	rate := p.usdRate
	fetched := p.fetched
	p.mu.RUnlock()

	if !fetched.IsZero() && time.Since(fetched) < p.ttl {
		return rate, nil
	}

	fresh, err := p.fetch(ctx)
	if err != nil {
		if !fetched.IsZero() {
			slog.Warn("CBR rate fetch failed, using stale cache", "error", err, "stale_rate", rate)
			return rate, nil
		}
		return 0, err
	}

	p.mu.Lock()
	p.usdRate = fresh
	p.fetched = time.Now()
	p.mu.Unlock()
	return fresh, nil
}

func (p *Provider) fetch(ctx context.Context) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cbrURL, nil)
	if err != nil {
		return 0, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("cbr returned HTTP %d", resp.StatusCode)
	}

	var payload struct {
		Valute map[string]struct {
			Value   float64 `json:"Value"`
			Nominal int     `json:"Nominal"`
		} `json:"Valute"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, err
	}

	usd, ok := payload.Valute["USD"]
	if !ok || usd.Nominal == 0 {
		return 0, errors.New("cbr response missing USD entry")
	}

	rate := usd.Value / float64(usd.Nominal)
	slog.Info("Updated USD rate from CBR", "rate", rate)
	return rate, nil
}
