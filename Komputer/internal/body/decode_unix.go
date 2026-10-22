//go:build !windows

package body

func detectConsoleEncoding() string { return "utf-8" }
