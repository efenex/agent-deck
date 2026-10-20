//go:build windows

package testutil

// startTmuxDirWatchdog is a no-op where there is no sh/tmux to supervise.
func startTmuxDirWatchdog(string) func() { return func() {} }

// ReapHomeOnExit is a no-op where there is no sh to supervise.
func ReapHomeOnExit(string) func() { return func() {} }

// ReapTmuxDirOnExit is a no-op where there is no sh to supervise.
func ReapTmuxDirOnExit(string) func() { return func() {} }
