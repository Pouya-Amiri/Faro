package updater

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	repository = "Pouya-Amiri/Faro"
	latestAPI  = "https://api.github.com/repos/" + repository + "/releases/latest"
	// ReleasesURL is the release page used when no specific release is known.
	ReleasesURL = "https://github.com/" + repository + "/releases/latest"
	// Release assets are only ever downloaded from this repository's own
	// release storage, whatever the API response says.
	assetURLPrefix = "https://github.com/" + repository + "/releases/download/"
	releasePageURL = "https://github.com/" + repository + "/releases/"
	checksumsName  = "SHA256SUMS.txt"

	maximumReleaseJSON = 4 << 20
	maximumNotes       = 16 << 10
	maximumChecksums   = 64 << 10
)

// release is the newest published GitHub release.
type release struct {
	version   version
	notes     string
	url       string
	published time.Time
	assets    map[string]asset
}

type asset struct {
	name string
	size int64
	url  string
}

func fetchLatest(ctx context.Context, client *http.Client, endpoint, userAgent, assetPrefix string) (release, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return release{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", userAgent)
	response, err := client.Do(request)
	if err != nil {
		return release{}, fmt.Errorf("could not reach GitHub: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		if response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusTooManyRequests {
			return release{}, errors.New("GitHub is limiting requests right now; try again later")
		}
		return release{}, fmt.Errorf("GitHub returned %s", response.Status)
	}
	var payload struct {
		TagName     string    `json:"tag_name"`
		Body        string    `json:"body"`
		HTMLURL     string    `json:"html_url"`
		PublishedAt time.Time `json:"published_at"`
		Draft       bool      `json:"draft"`
		Prerelease  bool      `json:"prerelease"`
		Assets      []struct {
			Name  string `json:"name"`
			Size  int64  `json:"size"`
			State string `json:"state"`
			URL   string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maximumReleaseJSON)).Decode(&payload); err != nil {
		return release{}, fmt.Errorf("read the latest release: %w", err)
	}
	if payload.Draft || payload.Prerelease {
		return release{}, errors.New("the latest release is not published yet")
	}
	parsed, err := parseVersion(payload.TagName)
	if err != nil {
		return release{}, fmt.Errorf("read the latest release version: %w", err)
	}
	result := release{
		version:   parsed,
		notes:     truncateUTF8(strings.TrimSpace(payload.Body), maximumNotes),
		url:       ReleasesURL,
		published: payload.PublishedAt,
		assets:    map[string]asset{},
	}
	if strings.HasPrefix(payload.HTMLURL, releasePageURL) {
		result.url = payload.HTMLURL
	}
	for _, item := range payload.Assets {
		if item.State != "" && item.State != "uploaded" {
			continue
		}
		if item.Name == "" || strings.ContainsAny(item.Name, `/\`) || !strings.HasPrefix(item.URL, assetPrefix) {
			continue
		}
		result.assets[item.Name] = asset{name: item.Name, size: item.Size, url: item.URL}
	}
	return result, nil
}

// fetchChecksums downloads and parses the release's SHA256SUMS.txt.
func fetchChecksums(ctx context.Context, client *http.Client, sums asset, userAgent string) (map[string]string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, sums.url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", userAgent)
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download checksums: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download checksums: server returned %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumChecksums+1))
	if err != nil {
		return nil, fmt.Errorf("download checksums: %w", err)
	}
	if len(data) > maximumChecksums {
		return nil, errors.New("the release checksum list is too large")
	}
	return parseChecksums(data), nil
}

// parseChecksums reads sha256sum output: a hash, whitespace and a file name,
// which binary mode prefixes with "*".
func parseChecksums(data []byte) map[string]string {
	sums := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		hash, name, found := strings.Cut(strings.TrimSpace(scanner.Text()), " ")
		if !found {
			continue
		}
		name = strings.TrimPrefix(strings.TrimSpace(name), "*")
		if decoded, err := hex.DecodeString(hash); err != nil || len(decoded) != 32 || name == "" {
			continue
		}
		sums[name] = strings.ToLower(hash)
	}
	return sums
}

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	// Only the cut can leave an incomplete character, at the very end.
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + "…"
}
