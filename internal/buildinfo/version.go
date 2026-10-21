package buildinfo

// Version is Faro's sole version source and may be replaced with -ldflags.
var Version = "1.2.1"

func EffectiveVersion() string {
	return Version
}
