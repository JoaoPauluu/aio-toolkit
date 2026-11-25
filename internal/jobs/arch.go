package jobs

import "runtime"

// Arch is the CPU architecture in the names release assets usually use
// (amd64, arm64, …), exposed to plugins as AIO_ARCH.
func Arch() string { return runtime.GOARCH }
