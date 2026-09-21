package main

import (
	"fmt"

	"github.com/Pouya-Amiri/Faro/internal/buildinfo"
)

func main() { fmt.Println(buildinfo.EffectiveVersion()) }
