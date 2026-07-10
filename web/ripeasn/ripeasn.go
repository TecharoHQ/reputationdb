package ripeasn

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"

	"github.com/TecharoHQ/reputationdb/web/useragent"
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

func Fetch(ctx context.Context, asNumber uint) (*Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://stat.ripe.net/data/announced-prefixes/data.json?resource=AS%d", asNumber), nil)
	if err != nil {
		return nil, fmt.Errorf("can't make request: %w", err)
	}

	req.Header.Set("User-Agent", useragent.GenUserAgent("TecharoHQ/reputationdb", "https://techaro.lol/contact"))

	resp, err := http.DefaultClient.Do(req)
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
