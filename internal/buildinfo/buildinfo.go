package buildinfo

import "strings"

var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

func DisplayVersion() string {
	version := strings.TrimSpace(Version)
	if version == "" {
		return "dev"
	}
	return strings.TrimPrefix(version, "v")
}
