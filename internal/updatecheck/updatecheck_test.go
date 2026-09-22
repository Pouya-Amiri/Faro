package updatecheck

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

func TestCheckReportsNewerRelease(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Accept") != "application/vnd.github+json" || request.Header.Get("User-Agent") != "Faro/1.0.3" {
			t.Fatalf("missing GitHub request headers: %#v", request.Header)
		}
		return &http.Response{
			StatusCode: http.StatusOK, Status: "200 OK",
			Body: io.NopCloser(strings.NewReader(`{"tag_name":"v1.1.0"}`)),
		}, nil
	})}
	status, err := check(context.Background(), client, "https://example.test/latest", "1.0.3")
	if err != nil {
		t.Fatal(err)
	}
	if !status.Available || status.CurrentVersion != "1.0.3" || status.LatestVersion != "1.1.0" || status.ReleaseURL != ReleasePageURL {
		t.Fatalf("unexpected update status: %#v", status)
	}
}

func TestCheckIgnoresSameOrOlderRelease(t *testing.T) {
	for _, tag := range []string{"v1.0.3", "v1.0.2", "v1.0.3-rc.1"} {
		t.Run(tag, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK, Status: "200 OK",
					Body: io.NopCloser(strings.NewReader(`{"tag_name":"` + tag + `"}`)),
				}, nil
			})}
			status, err := check(context.Background(), client, "https://example.test/latest", "1.0.3")
			if err != nil {
				t.Fatal(err)
			}
			if status.Available {
				t.Fatalf("release %s was incorrectly newer than 1.0.3", tag)
			}
		})
	}
}

func TestCheckRejectsInvalidResponses(t *testing.T) {
	tests := []struct {
		name, body, status string
		code               int
	}{
		{name: "http error", code: http.StatusForbidden, status: "403 Forbidden", body: `{}`},
		{name: "invalid json", code: http.StatusOK, status: "200 OK", body: `{`},
		{name: "invalid tag", code: http.StatusOK, status: "200 OK", body: `{"tag_name":"latest"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: test.code, Status: test.status, Body: io.NopCloser(strings.NewReader(test.body))}, nil
			})}
			if _, err := check(context.Background(), client, "https://example.test/latest", "1.0.3"); err == nil {
				t.Fatal("invalid release response was accepted")
			}
		})
	}
}

func TestSemanticVersionPrecedence(t *testing.T) {
	ordered := []string{
		"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta",
		"1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.1.0", "2.0.0",
	}
	for index := 1; index < len(ordered); index++ {
		previous, err := parseVersion(ordered[index-1])
		if err != nil {
			t.Fatal(err)
		}
		current, err := parseVersion(ordered[index])
		if err != nil {
			t.Fatal(err)
		}
		if current.compare(previous) <= 0 {
			t.Fatalf("%s did not sort after %s", current, previous)
		}
	}
}

func TestSemanticVersionSupportsLargeNumericIdentifiers(t *testing.T) {
	left, err := parseVersion("18446744073709551616.0.0-999999999999999999999")
	if err != nil {
		t.Fatal(err)
	}
	right, err := parseVersion("18446744073709551615.9.9-1")
	if err != nil {
		t.Fatal(err)
	}
	if left.compare(right) <= 0 {
		t.Fatalf("%s did not sort after %s", left, right)
	}
}

func TestSemanticVersionRejectsMalformedValues(t *testing.T) {
	for _, value := range []string{"1.0", "1.0.0-", "1.0.0+", "1.0.0-alpha..1", "1.0.0+build..1", "1.0.0-01"} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseVersion(value); err == nil {
				t.Fatalf("malformed version %q was accepted", value)
			}
		})
	}
}
