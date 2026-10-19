//go:build !darwin && !windows && !linux

package sysproxy

// systemSettings returns no proxy on platforms this package does not know;
// the environment is still honoured.
func systemSettings() settings { return settings{} }
