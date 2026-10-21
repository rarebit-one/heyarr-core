package identification

import (
	"path"
	"strings"
)

// executableExts name a file that only ever exists to be run. A release whose
// payload is one of these, wearing an episode's or a film's name, is a fake
// (#819); for a game the same files are its installers.
var executableExts = newSet(".exe", ".msi", ".scr", ".bat", ".cmd", ".com", ".lnk")

// IsExecutableName reports whether a file name (or a slash-separated path
// inside a release) ends in an executable's extension.
func IsExecutableName(name string) bool {
	return executableExts[strings.ToLower(path.Ext(strings.ReplaceAll(name, "\\", "/")))]
}
