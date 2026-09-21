package frontend

import (
	"embed"
	"io/fs"
)

// Assets contains the dependency-free desktop frontend.
//
//go:embed dist/*
var embedded embed.FS

var Assets = mustSub(embedded, "dist")

func mustSub(files embed.FS, directory string) fs.FS {
	result, err := fs.Sub(files, directory)
	if err != nil {
		panic(err)
	}
	return result
}
