//go:build !darwin && !linux && !windows

package traekey

import "context"

// Supported reports whether this build can read a key out of memory.
func Supported() bool { return false }

// Capture is not implemented on this platform. The key has to be pasted from a
// tool that already recovers one, such as Oh-My-Trae/trae-db-decrypt.
func Capture(context.Context, Options) (string, error) { return "", ErrUnsupported }
