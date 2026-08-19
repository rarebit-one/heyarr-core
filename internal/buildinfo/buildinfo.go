package buildinfo

import "runtime/debug"

// Injected at link time by goreleaser; see .goreleaser.yaml.
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// Info is the resolved build identity of this binary.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"go_version"`
}

// Get returns the build identity, falling back to the embedded VCS stamps Go
// records for `go build` and `go install` builds when ldflags were not set.
func Get() Info {
	i := Info{Version: Version, Commit: Commit, Date: Date}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return i
	}
	i.GoVersion = bi.GoVersion
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if i.Commit == "" {
				i.Commit = s.Value
			}
		case "vcs.time":
			if i.Date == "" {
				i.Date = s.Value
			}
		}
	}
	return i
}
