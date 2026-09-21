// Command faro-release keeps generated release metadata aligned with the
// application version and validates release tags.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/Pouya-Amiri/Faro/internal/buildinfo"
)

var (
	versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
	// Do not anchor at the end of the line: on a CRLF checkout the value is
	// followed by \r before \n. The character class stops before either line
	// ending, so replacing the match preserves the file's original EOL style.
	configPattern    = regexp.MustCompile(`(?m)^  version: [^\r\n]+`)
	appstreamPattern = regexp.MustCompile(`<release version="[^"]+" date="[^"]+">`)
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "faro-release:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if !versionPattern.MatchString(buildinfo.EffectiveVersion()) {
		return fmt.Errorf("invalid semantic version %q", buildinfo.EffectiveVersion())
	}

	if len(args) == 2 && args[0] == "check-tag" {
		expected := "v" + buildinfo.EffectiveVersion()
		if args[1] != expected {
			return fmt.Errorf("tag %q does not match version %q (expected %q)", args[1], buildinfo.EffectiveVersion(), expected)
		}
		return nil
	}
	if len(args) == 1 && args[0] == "sync" {
		return syncMetadata(time.Now().UTC().Format(time.DateOnly))
	}

	return errors.New("usage: faro-release sync | faro-release check-tag <tag>")
}

func syncMetadata(date string) error {
	version := buildinfo.EffectiveVersion()
	if err := replaceExactlyOnce(
		"build/config.yml",
		configPattern,
		[]byte("  version: "+version),
	); err != nil {
		return err
	}
	return replaceExactlyOnce(
		"packaging/linux/io.github.pouya_amiri.Faro.metainfo.xml",
		appstreamPattern,
		[]byte(fmt.Sprintf(`<release version="%s" date="%s">`, version, date)),
	)
}

func replaceExactlyOnce(path string, pattern *regexp.Regexp, replacement []byte) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if matches := pattern.FindAllIndex(data, -1); len(matches) != 1 {
		return fmt.Errorf("expected exactly one generated version field in %s, found %d", path, len(matches))
	}

	updated := pattern.ReplaceAll(data, replacement)
	if bytes.Equal(data, updated) {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if err := os.WriteFile(path, updated, info.Mode().Perm()); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
