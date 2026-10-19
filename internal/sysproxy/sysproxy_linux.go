//go:build linux

package sysproxy

// systemSettings returns no proxy. Linux has no system-wide proxy setting that
// can be read without a desktop-specific API, so the environment is the only
// source there.
func systemSettings() settings { return settings{} }
