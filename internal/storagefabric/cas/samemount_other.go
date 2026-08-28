//go:build !linux

package cas

// SameMount off Linux is SameFilesystem. The #222 failure is specific to Linux
// bind mounts under a ProtectSystem=strict systemd unit; elsewhere there is no
// way for two paths on one device to be on different mounts that link(2) would
// refuse, so the device number answers the question correctly.
func SameMount(a, b string) (same, known bool, err error) {
	return SameFilesystem(a, b)
}
