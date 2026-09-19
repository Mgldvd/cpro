package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const usageEndpoint = "https://api.anthropic.com/api/oauth/usage?at_wall=1&skip_spend=1"

type usageWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

type accountUsage struct {
	FiveHour usageWindow `json:"five_hour"`
	SevenDay usageWindow `json:"seven_day"`
}

type usageCache struct {
	FetchedAt time.Time    `json:"fetched_at"`
	Usage     accountUsage `json:"usage"`
}

func loadUsage(profile, endpoint string) (accountUsage, bool, error) {
	cachePath := filepath.Join(profile, "cpro-usage.json")
	cached, cacheErr := readUsageCache(cachePath)
	if cacheErr == nil && time.Since(cached.FetchedAt) < time.Minute {
		return cached.Usage, false, nil
	}

	usage, err := fetchUsage(profile, endpoint)
	if err != nil {
		if cacheErr == nil {
			return cached.Usage, true, nil
		}
		return usage, false, err
	}
	b, err := json.Marshal(usageCache{FetchedAt: time.Now(), Usage: usage})
	if err == nil {
		err = atomicWrite(cachePath, append(b, '\n'))
	}
	return usage, false, err
}

func readUsageCache(path string) (usageCache, error) {
	var cached usageCache
	b, err := os.ReadFile(path)
	if err != nil {
		return cached, err
	}
	err = json.Unmarshal(b, &cached)
	return cached, err
}

func fetchUsage(profile, endpoint string) (accountUsage, error) {
	var credentials struct {
		ClaudeAI struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	var usage accountUsage
	b, err := os.ReadFile(filepath.Join(profile, ".credentials.json"))
	if err != nil {
		return usage, err
	}
	if err := json.Unmarshal(b, &credentials); err != nil || credentials.ClaudeAI.AccessToken == "" {
		return usage, fmt.Errorf("OAuth credentials unavailable")
	}

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return usage, err
	}
	req.Header.Set("Authorization", "Bearer "+credentials.ClaudeAI.AccessToken)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", "cpro/"+version)
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return usage, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return usage, fmt.Errorf("usage request returned HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(&usage); err != nil {
		return usage, err
	}
	return usage, nil
}
