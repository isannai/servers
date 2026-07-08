//go:build !windows

package glog

import "io"

// wrapConsole is a no-op on Unix-like OSes. Linux / macOS terminals
// handle bare \n correctly (the kernel ONLCR mode adds the \r when the
// fd is a tty); only Windows console needs explicit \r\n.
func wrapConsole(w io.Writer) io.Writer { return w }
