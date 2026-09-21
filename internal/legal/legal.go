package legal

import (
	_ "embed"
	"net/url"
	"strings"
)

const (
	LicenseName   = "Mozilla Public License 2.0"
	repositoryURL = "https://github.com/Pouya-Amiri/Faro"
)

//go:generate go run ./generate

// The legal texts are embedded so they remain available when Faro is
// distributed as a single executable.
//
//go:embed assets/MPL-2.0.txt
var projectLicense string

//go:embed assets/THIRD_PARTY_NOTICES.txt
var thirdPartyNotices string

type Information struct {
	Version           string `json:"version"`
	LicenseName       string `json:"licenseName"`
	ProjectLicense    string `json:"projectLicense"`
	ThirdPartyNotices string `json:"thirdPartyNotices"`
	SourceURL         string `json:"sourceUrl"`
}

func Info(version string) Information {
	return Information{
		Version: version, LicenseName: LicenseName,
		ProjectLicense: projectLicense, ThirdPartyNotices: thirdPartyNotices,
		SourceURL: sourceURL(version),
	}
}

func sourceURL(version string) string {
	version = strings.TrimSpace(strings.TrimPrefix(version, "v"))
	if version == "" || strings.Contains(version, "dev") {
		return repositoryURL
	}
	return repositoryURL + "/tree/v" + url.PathEscape(version)
}
