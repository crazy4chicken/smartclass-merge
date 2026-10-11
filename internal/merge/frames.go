package merge

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// framePrefixBytes is the big-endian length prefix in front of every encoded
// frame inside a segment object (recorder protocol, contract §1).
const framePrefixBytes = 4

// errFraming marks a segment body that is not a run of length-prefixed frames: a
// truncated length prefix, a truncated frame or a zero-length frame.
var errFraming = errors.New("malformed segment framing")

// SplitFrames copies the encoded frames of one segment object to dst, stripping
// the framing, and reports how many frames and payload bytes it copied (contract
// §5.5). Reading stops at a clean end of stream; trailing garbage, a frame the
// segment does not carry in full and a zero-length frame are errors wrapping
// errFraming, so a damaged segment never reaches ffmpeg unnoticed.
//
// A write failure is returned as it is: when ffmpeg has already exited, its
// broken pipe fails the merge and the framing of the segment is not at fault.
func SplitFrames(dst io.Writer, src io.Reader) (frames, payloadBytes int64, err error) {
	var prefix [framePrefixBytes]byte
	for {
		if _, err := io.ReadFull(src, prefix[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return frames, payloadBytes, nil
			}
			return frames, payloadBytes, fmt.Errorf("%w: truncated length prefix after %d payload bytes", errFraming, payloadBytes)
		}
		size := int64(binary.BigEndian.Uint32(prefix[:]))
		if size == 0 {
			return frames, payloadBytes, fmt.Errorf("%w: zero-length frame after %d payload bytes", errFraming, payloadBytes)
		}
		written, err := io.CopyN(dst, src, size)
		payloadBytes += written
		switch {
		case errors.Is(err, io.EOF):
			return frames, payloadBytes, fmt.Errorf("%w: frame %d truncated after %d of %d bytes", errFraming, frames+1, written, size)
		case err != nil:
			return frames, payloadBytes, fmt.Errorf("copy frame %d: %w", frames+1, err)
		}
		frames++
	}
}

// collectFrames returns the concatenated payloads of the first frames of src, up
// to limit bytes, so Sniff can inspect the parameter sets a raw elementary
// stream carries in its leading frames. It stops at the limit even in the middle
// of a frame: the caller reads one bounded window of a segment that the merge
// streams again afterwards.
func collectFrames(src io.Reader, limit int) ([]byte, error) {
	var (
		prefix [framePrefixBytes]byte
		out    bytes.Buffer
	)
	for out.Len() < limit {
		if _, err := io.ReadFull(src, prefix[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return out.Bytes(), nil
			}
			return nil, fmt.Errorf("%w: truncated length prefix", errFraming)
		}
		size := int64(binary.BigEndian.Uint32(prefix[:]))
		if size == 0 {
			return nil, fmt.Errorf("%w: zero-length frame", errFraming)
		}
		if want := int64(limit - out.Len()); size > want {
			size = want
		}
		if _, err := io.CopyN(&out, src, size); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("%w: frame truncated after %d bytes", errFraming, out.Len())
			}
			return nil, err
		}
	}
	return out.Bytes(), nil
}
