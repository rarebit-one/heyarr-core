//go:build !unix

package cas

import "io/fs"

// owner has nothing portable to report on this platform: ownership is an ACL
// question here, and a numeric uid would be a fiction.
func owner(fs.FileInfo) string { return "" }
