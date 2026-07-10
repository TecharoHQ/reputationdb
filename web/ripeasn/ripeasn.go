package ripeasn

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
)

type Response struct {
	Messages       [][]string `json:"messages"`
	SeeAlso        []any      `json:"see_also"`
	Version        string     `json:"version"`
	DataCallName   string     `json:"data_call_name"`
	DataCallStatus string     `json:"data_call_status"`
	Cached         bool       `json:"cached"`
	Data           struct {
		Prefixes []struct {
			Prefix netip.Prefix `json:"prefix"`
		} `json:"prefixes"`
	} `json:"data"`
	Status     string `json:"status"`
	StatusCode int    `json:"status_code"`
}

// Fetch retrieves the prefixes announced by asNumber from RIPEstat using client.
// The caller is responsible for tagging outgoing requests with a User-Agent,
// typically by wrapping client's transport with [useragent.Transport].
func Fetch(ctx context.Context, client *http.Client, asNumber uint) (*Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://stat.ripe.net/data/announced-prefixes/data.json?resource=AS%d", asNumber), nil)
	if err != nil {
		return nil, fmt.Errorf("can't make request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("can't get details for AS%d: %w", asNumber, err)
	}
	defer resp.Body.Close()

	var result Response
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("can't read details for AS%d: %w", asNumber, err)
	}

	return &result, nil
}
