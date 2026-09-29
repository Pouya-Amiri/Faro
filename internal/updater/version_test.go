package updater

import "testing"

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
