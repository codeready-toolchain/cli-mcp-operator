package version

import "runtime/debug"

// commitOverride is set via -ldflags at build time for container builds
// where .git is unavailable.
var commitOverride string

// Commit is the short git commit hash (up to 8 chars) from build info.
var Commit = initCommit()

// BuildTime is set via -ldflags at build time.
var BuildTime = "unknown"

func initCommit() string {
	if commitOverride != "" {
		if len(commitOverride) > 8 {
			return commitOverride[:8]
		}
		return commitOverride
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			if len(s.Value) > 8 {
				return s.Value[:8]
			}
			return s.Value
		}
	}
	return "dev"
}
