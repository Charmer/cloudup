// Package streamio provides small io.Reader/io.Writer wrappers shared by
// provider implementations for reporting upload/download progress without
// altering the underlying byte stream (safe to further wrap in
// io.TeeReader for checksum computation), plus the concurrency engine
// (parallel.go) shared by every provider.ParallelMultipartUploader
// implementation.
package streamio

import "io"

// ProgressReader wraps an io.Reader and reports cumulative bytes read via
// OnProgress.
type ProgressReader struct {
	R          io.Reader
	Total      int64
	sent       int64
	OnProgress func(sent, total int64)
}

func (p *ProgressReader) Read(buf []byte) (int, error) {
	n, err := p.R.Read(buf)
	if n > 0 {
		p.sent += int64(n)
		if p.OnProgress != nil {
			p.OnProgress(p.sent, p.Total)
		}
	}
	return n, err
}

// ProgressWriter wraps an io.Writer and reports cumulative bytes written
// via OnProgress.
type ProgressWriter struct {
	W          io.Writer
	Total      int64
	received   int64
	OnProgress func(received, total int64)
}

func (p *ProgressWriter) Write(buf []byte) (int, error) {
	n, err := p.W.Write(buf)
	if n > 0 {
		p.received += int64(n)
		if p.OnProgress != nil {
			p.OnProgress(p.received, p.Total)
		}
	}
	return n, err
}
