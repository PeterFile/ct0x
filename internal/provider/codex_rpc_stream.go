package provider

import (
	"bufio"
	"errors"
	"io"
)

var errCodexRPCRead = errors.New("读取 Codex RPC 流失败")

// Experimental private JSONL reader; one owner calls Next serially.
// The caller owns the source lifecycle, including unblocking reads.
type codexRPCReader struct {
	input *bufio.Reader
	frame []byte
	err   error
}

func newCodexRPCReader(source io.Reader) *codexRPCReader {
	// The wrapper prevents reuse of a caller's potentially larger bufio.Reader.
	return &codexRPCReader{input: bufio.NewReaderSize(codexRPCInput{source}, 4<<10)}
}

func (r *codexRPCReader) Next() (codexRPCMessage, error) {
	if r.err != nil {
		return codexRPCMessage{}, r.err
	}
	r.frame = r.frame[:0]
	for {
		part, err := r.input.ReadSlice('\n')
		if err == nil {
			part = part[:len(part)-1] // LF is framing; CR remains in the payload budget.
		}
		if len(part) > codexRPCFrameLimit-len(r.frame) {
			r.err = errCodexRPCSize
			break
		}
		size := len(r.frame) + len(part)
		if size > cap(r.frame) {
			// Do not let append's growth policy retain capacity above the limit.
			frame := make([]byte, len(r.frame), min(codexRPCFrameLimit, max(size, 2*cap(r.frame))))
			copy(frame, r.frame)
			r.frame = frame
		}
		r.frame = append(r.frame, part...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			// Requiring a final LF is a host policy, not an upstream restriction.
			if err == io.EOF && len(r.frame) != 0 {
				err = io.ErrUnexpectedEOF
			}
			r.err = err
			break
		}
		message, err := decodeCodexRPCFrame(r.frame)
		if err != nil {
			r.err = err
			break
		}
		return message, nil
	}
	return codexRPCMessage{}, r.err
}

type codexRPCInput struct{ io.Reader }

func (source codexRPCInput) Read(p []byte) (int, error) {
	n, err := source.Reader.Read(p)
	// Normalize before bufio sees the error: a source's ErrBufferFull is not
	// bufio's internal fragment signal, and arbitrary errors may contain data.
	switch {
	case err == nil:
	case errors.Is(err, io.EOF):
		err = io.EOF
	case errors.Is(err, io.ErrUnexpectedEOF):
		err = io.ErrUnexpectedEOF
	case errors.Is(err, io.ErrNoProgress):
		err = io.ErrNoProgress
	default:
		err = errCodexRPCRead
	}
	return n, err
}
