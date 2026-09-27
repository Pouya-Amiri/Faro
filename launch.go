package main

import (
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// launchPaths picks the media files and folders out of a command line, as a
// file manager's "Open With Faro" passes them (see faro.desktop). Relative
// paths are resolved against directory, the launching process's working
// directory. Options and anything that does not exist are ignored.
func launchPaths(args []string, directory string) []string {
	paths := []string{}
	for _, arg := range args {
		arg = strings.TrimSpace(arg)
		if arg == "" || strings.HasPrefix(arg, "-") {
			continue
		}
		path := arg
		if parsed, err := url.Parse(arg); err == nil && parsed.Scheme == "file" {
			decoded, err := url.PathUnescape(parsed.EscapedPath())
			if err != nil {
				continue
			}
			path = filepath.FromSlash(decoded)
			if runtime.GOOS == "windows" {
				path = strings.TrimPrefix(path, `\`)
			}
		} else if strings.Contains(arg, "://") {
			continue
		}
		if !filepath.IsAbs(path) && directory != "" {
			path = filepath.Join(directory, path)
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() && !info.IsDir() {
			continue
		}
		if absolute, err := filepath.Abs(path); err == nil {
			path = absolute
		}
		paths = append(paths, path)
	}
	return paths
}

// queueLaunchPaths keeps opened files until the page collects them with
// TakeLaunchPaths, and tells a page that is already running to do so.
func (d *Desktop) queueLaunchPaths(paths []string) {
	if len(paths) == 0 {
		return
	}
	d.launchMu.Lock()
	d.launchPaths = append(d.launchPaths, paths...)
	d.launchMu.Unlock()
	if d.app != nil {
		d.app.Event.Emit("faro:open-files", nil)
	}
}

// TakeLaunchPaths returns and forgets the files Faro was asked to open.
func (d *Desktop) TakeLaunchPaths() []string {
	d.launchMu.Lock()
	defer d.launchMu.Unlock()
	paths := d.launchPaths
	d.launchPaths = nil
	if paths == nil {
		return []string{}
	}
	return paths
}

func workingDirectory() string {
	directory, _ := os.Getwd()
	return directory
}
