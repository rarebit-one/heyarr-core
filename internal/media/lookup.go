package media

// NoToolchain is Options describing a machine with neither binary on PATH.
//
// It exists so that other packages can test their behaviour on a bare node.
// That case is the one ADR-0023 turns on, and it is untestable by ordinary
// means on a machine that HAS FFmpeg — which CI's Linux runners deliberately
// do. Without this, the degrade path would be exercised only where the binary
// happened to be missing, which is to say by accident.
func NoToolchain() Options {
	return Options{LookPath: func(name string) (string, error) {
		return "", &notFoundError{name: name}
	}}
}

type notFoundError struct{ name string }

func (e *notFoundError) Error() string {
	return "exec: " + e.name + `: executable file not found in $PATH`
}
