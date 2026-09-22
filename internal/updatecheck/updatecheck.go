package updatecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	latestReleaseAPI = "https://api.github.com/repos/Pouya-Amiri/Faro/releases/latest"
	ReleasePageURL   = "https://github.com/Pouya-Amiri/Faro/releases/latest"
	maximumResponse  = 1 << 20
)

type Status struct {
	CurrentVersion string `json:"currentVersion"`
	LatestVersion  string `json:"latestVersion"`
	Available      bool   `json:"available"`
	ReleaseURL     string `json:"releaseUrl"`
}

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

func Check(ctx context.Context, currentVersion string) (Status, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	return check(ctx, client, latestReleaseAPI, currentVersion)
}

func check(ctx context.Context, client httpDoer, endpoint, currentVersion string) (Status, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	current, err := parseVersion(currentVersion)
	if err != nil {
		return Status{}, fmt.Errorf("parse current version: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Status{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "Faro/"+currentVersion)
	response, err := client.Do(request)
	if err != nil {
		return Status{}, fmt.Errorf("check latest Faro release: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return Status{}, fmt.Errorf("check latest Faro release: GitHub returned %s", response.Status)
	}
	var release struct {
		TagName string `json:"tag_name"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maximumResponse))
	if err := decoder.Decode(&release); err != nil {
		return Status{}, fmt.Errorf("decode latest Faro release: %w", err)
	}
	latest, err := parseVersion(release.TagName)
	if err != nil {
		return Status{}, fmt.Errorf("parse latest Faro release: %w", err)
	}
	return Status{
		CurrentVersion: current.String(),
		LatestVersion:  latest.String(),
		Available:      latest.compare(current) > 0,
		ReleaseURL:     ReleasePageURL,
	}, nil
}

type version struct {
	major, minor, patch string
	prerelease          []string
}

func parseVersion(value string) (version, error) {
	value = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "v"))
	if core, build, found := strings.Cut(value, "+"); found {
		if !validIdentifiers(build, true) {
			return version{}, errors.New("build metadata is invalid")
		}
		value = core
	}
	main, prerelease, hasPrerelease := strings.Cut(value, "-")
	parts := strings.Split(main, ".")
	if len(parts) != 3 {
		return version{}, errors.New("version must contain major, minor, and patch numbers")
	}
	for _, part := range parts {
		if !numeric(part) || len(part) > 1 && part[0] == '0' {
			return version{}, errors.New("version number is empty or has a leading zero")
		}
	}
	result := version{major: parts[0], minor: parts[1], patch: parts[2]}
	if hasPrerelease {
		if !validIdentifiers(prerelease, false) {
			return version{}, errors.New("prerelease is invalid")
		}
		result.prerelease = strings.Split(prerelease, ".")
	}
	return result, nil
}

func (v version) String() string {
	value := fmt.Sprintf("%s.%s.%s", v.major, v.minor, v.patch)
	if len(v.prerelease) != 0 {
		value += "-" + strings.Join(v.prerelease, ".")
	}
	return value
}

func (v version) compare(other version) int {
	for _, pair := range [][2]string{{v.major, other.major}, {v.minor, other.minor}, {v.patch, other.patch}} {
		if compared := compareNumeric(pair[0], pair[1]); compared != 0 {
			return compared
		}
	}
	if len(v.prerelease) == 0 || len(other.prerelease) == 0 {
		if len(v.prerelease) == len(other.prerelease) {
			return 0
		}
		if len(v.prerelease) == 0 {
			return 1
		}
		return -1
	}
	for index := 0; index < min(len(v.prerelease), len(other.prerelease)); index++ {
		left, right := v.prerelease[index], other.prerelease[index]
		leftNumeric, rightNumeric := numeric(left), numeric(right)
		if leftNumeric && rightNumeric {
			if compared := compareNumeric(left, right); compared != 0 {
				return compared
			}
			continue
		}
		if leftNumeric != rightNumeric {
			if leftNumeric {
				return -1
			}
			return 1
		}
		if left < right {
			return -1
		}
		if left > right {
			return 1
		}
	}
	if len(v.prerelease) < len(other.prerelease) {
		return -1
	}
	if len(v.prerelease) > len(other.prerelease) {
		return 1
	}
	return 0
}

func validIdentifiers(value string, build bool) bool {
	for _, identifier := range strings.Split(value, ".") {
		if identifier == "" || !validIdentifier(identifier) {
			return false
		}
		if !build && numeric(identifier) && len(identifier) > 1 && identifier[0] == '0' {
			return false
		}
	}
	return true
}

func validIdentifier(value string) bool {
	for _, current := range value {
		if current >= 'a' && current <= 'z' || current >= 'A' && current <= 'Z' || current >= '0' && current <= '9' || current == '-' {
			continue
		}
		return false
	}
	return true
}

func compareNumeric(left, right string) int {
	if len(left) < len(right) {
		return -1
	}
	if len(left) > len(right) {
		return 1
	}
	return strings.Compare(left, right)
}

func numeric(value string) bool {
	for _, current := range value {
		if current < '0' || current > '9' {
			return false
		}
	}
	return value != ""
}
