package b2

import (
	"crypto/sha1"
	"encoding/hex"
	"hash"
	"io"
)

// sha1TrailerReader wraps an io.Reader and, once the underlying reader is
// fully drained, appends the lowercase hex SHA-1 digest of everything that
// was read as 40 extra trailing bytes before finally returning io.EOF.
//
// This exists because B2's upload API lets a client stream a file whose
// SHA-1 isn't known up front (as is the case here: task.Reader is only
// read once, so nothing can be hashed in advance without buffering the
// whole file in memory) by sending the literal header value
// "hex_digits_at_end" for X-Bz-Content-Sha1 and appending the 40 hex
// digest bytes as the last bytes of the request body - see
// https://www.backblaze.com/apidocs/b2-upload-file. Wrapping task.Reader
// in this type lets Upload hand a single io.Reader to net/http and get
// the trailer appended automatically, with no extra buffering pass.
type sha1TrailerReader struct {
	r    io.Reader
	h    hash.Hash
	done bool   // underlying r has returned EOF; now serving the trailer
	trl  []byte // the 40 hex trailer bytes, once computed
	pos  int    // how much of trl has already been served
}

// newSHA1TrailerReader wraps r so that reading it to EOF yields the
// original bytes of r followed by 40 bytes: the lowercase hex SHA-1 of
// everything read from r.
func newSHA1TrailerReader(r io.Reader) *sha1TrailerReader {
	h := sha1.New()
	return &sha1TrailerReader{r: io.TeeReader(r, h), h: h}
}

func (s *sha1TrailerReader) Read(buf []byte) (int, error) {
	if !s.done {
		n, err := s.r.Read(buf)
		if err == io.EOF {
			s.done = true
			s.trl = []byte(hex.EncodeToString(s.h.Sum(nil)))
			if n > 0 {
				// Report the final chunk of real data now; the trailer
				// itself is served starting on the next Read call.
				return n, nil
			}
			// Fall through to serve the trailer immediately.
		} else {
			return n, err
		}
	}

	if s.pos >= len(s.trl) {
		return 0, io.EOF
	}
	n := copy(buf, s.trl[s.pos:])
	s.pos += n
	if s.pos >= len(s.trl) {
		return n, io.EOF
	}
	return n, nil
}
