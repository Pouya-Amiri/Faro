package legal

import (
	"os"
	"strings"
	"testing"
)

func TestEmbeddedProjectLicenseMatchesRepositoryLicense(t *testing.T) {
	want, err := os.ReadFile("../../LICENSE")
	if err != nil {
		t.Fatal(err)
	}
	if projectLicense != string(want) {
		t.Fatal("embedded MPL-2.0 text differs from the repository LICENSE")
	}
}

func TestLegalInfoUsesReleaseSourceTag(t *testing.T) {
	release := Info("0.3.4")
	if release.SourceURL != repositoryURL+"/tree/v0.3.4" {
		t.Fatalf("unexpected release source URL %q", release.SourceURL)
	}
	development := Info("0.3.5-dev")
	if development.SourceURL != repositoryURL {
		t.Fatalf("unexpected development source URL %q", development.SourceURL)
	}
	if !strings.Contains(release.ThirdPartyNotices, "github.com/tailscale/tailcat") {
		t.Fatal("third-party notices omit Tailcat")
	}
}
