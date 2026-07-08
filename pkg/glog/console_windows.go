//go:build windows

package glog

import "io"

// crlfWriter rewrites bare \n into \r\n. Windows console relies on the
// explicit \r to reset the cursor to column 0; without it, log lines
// produced by the standard log package (which only appends \n) print at
// progressively shifted indentation. Already-CRLF input passes through
// unchanged so file output (utf8 with \r\n) doesn't get \r\r\n.
type crlfWriter struct{ w io.Writer }

func (c *crlfWriter) Write(p []byte) (int, error) {
	buf := make([]byte, 0, len(p)+8)
	for i, b := range p {
		if b == '\n' && (i == 0 || p[i-1] != '\r') {
			buf = append(buf, '\r')
		}
		buf = append(buf, b)
	}
	if _, err := c.w.Write(buf); err != nil {
		return 0, err
	}
	// io.Writer contract: report bytes consumed from p (not bytes written
	// underlying), so the caller's accounting stays consistent.
	return len(p), nil
}

func wrapConsole(w io.Writer) io.Writer { return &crlfWriter{w: w} }
