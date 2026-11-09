package provider

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

const codexRPCStreamCallback = `{"id":"rpc-next","method":"item/tool/call","params":{"callId":"business-next","threadId":"thread-1","turnId":"turn-1","tool":"workspace.read","arguments":{}}}` + "\n"

// Generate the oversized prefix without allocating a large fixture string.
type codexRPCStreamCountingReader struct {
	remaining                int
	tail                     io.Reader
	reads, bytes, maxRequest int
}

func (r *codexRPCStreamCountingReader) Read(p []byte) (int, error) {
	r.reads++
	r.maxRequest = max(r.maxRequest, len(p))
	var n int
	var err error
	if r.remaining > 0 {
		n = min(r.remaining, len(p))
		for i := range p[:n] {
			p[i] = 'x'
		}
		r.remaining -= n
	} else {
		n, err = r.tail.Read(p)
	}
	r.bytes += n
	return n, err
}

func codexRPCStreamTerminal(t *testing.T, r *codexRPCReader, source *codexRPCStreamCountingReader, want error) {
	t.Helper()
	reads, bytes := source.reads, source.bytes
	for range 2 {
		message, err := r.Next()
		if err != want || !reflect.DeepEqual(message, codexRPCMessage{}) {
			t.Errorf("terminal Next returned a message or changed error: %v, want %v", err, want)
		}
		if source.reads != reads || source.bytes != bytes {
			t.Errorf("terminal Next performed I/O: reads %d -> %d, bytes %d -> %d", reads, source.reads, bytes, source.bytes)
		}
	}
}

func TestCodexRPCStreamOversizeSticky(t *testing.T) {
	source := &codexRPCStreamCountingReader{
		remaining: 3 * codexRPCFrameLimit,
		tail:      strings.NewReader("\n" + codexRPCStreamCallback),
	}
	r := newCodexRPCReader(source)
	message, err := r.Next()
	if err != errCodexRPCSize || !reflect.DeepEqual(message, codexRPCMessage{}) {
		t.Errorf("oversized prefix returned a message or wrong error: %v", err)
	}
	if source.bytes > codexRPCFrameLimit+4<<10 {
		t.Errorf("unbounded source read: got %d bytes, budget %d", source.bytes, codexRPCFrameLimit+4<<10)
	}
	if source.maxRequest > 4<<10 {
		t.Errorf("source read request exceeded 4 KiB: %d", source.maxRequest)
	}
	if len(r.frame) > codexRPCFrameLimit || cap(r.frame) > codexRPCFrameLimit {
		t.Errorf("retained frame exceeded limit: len=%d cap=%d", len(r.frame), cap(r.frame))
	}
	codexRPCStreamTerminal(t, r, source, errCodexRPCSize)
}

func TestCodexRPCStreamMessages(t *testing.T) {
	// Handwritten pinned 0.166 envelopes; these are not native runtime traffic.
	params := `{"arguments":{"text":"中文😀\uD83D\uDE00\\uD800"},"callId":"business-call","namespace":null,"threadId":"thread-1","tool":"workspace.read","turnId":"turn-1"}`
	request, notification := "item/tool/call", "initialized"
	frames := []string{
		`{"id":"rpc-request","method":"item/tool/call","params":` + params + `}`,
		`{"method":"initialized"}`,
		`{"id":9007199254740993,"result":{"nested":[null,true]}}`,
		`{"id":"rpc-error","error":{"code":-32601,"message":"未知方法","data":null}}`,
	}
	want := []codexRPCMessage{
		{ID: json.RawMessage(`"rpc-request"`), Method: &request, Params: json.RawMessage(params)},
		{Method: &notification},
		{ID: json.RawMessage(`9007199254740993`), Result: json.RawMessage(`{"nested":[null,true]}`)},
		{ID: json.RawMessage(`"rpc-error"`), Error: json.RawMessage(`{"code":-32601,"message":"未知方法","data":null}`)},
	}
	for _, mode := range []string{"coalesced", "one-byte", "buffered-source"} {
		t.Run(mode, func(t *testing.T) {
			var input io.Reader = strings.NewReader(strings.Join(frames, "\n") + "\n")
			switch mode {
			case "one-byte":
				input = iotest.OneByteReader(input)
			case "buffered-source":
				input = bufio.NewReaderSize(input, 64<<10)
			}
			r := newCodexRPCReader(input)
			if r.input.Size() != 4<<10 || r.input == input {
				t.Fatal("reader did not allocate its own 4 KiB input buffer")
			}
			var got []codexRPCMessage
			for range frames {
				message, err := r.Next()
				if err != nil {
					t.Fatalf("complete frame rejected: %v", err)
				}
				got = append(got, message)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("stream order, raw fields, or earlier buffer ownership changed: got %+v", got)
			}
			if message, err := r.Next(); err != io.EOF || !reflect.DeepEqual(message, codexRPCMessage{}) {
				t.Fatalf("clean stream end: %v", err)
			}
		})
	}
}

func TestCodexRPCStreamBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		size   int
		ending string
		want   error
	}{
		{"limit", codexRPCFrameLimit, "\n", nil},
		{"plus-one", codexRPCFrameLimit + 1, "\n", errCodexRPCSize},
		{"CR-at-limit", codexRPCFrameLimit - 1, "\r\n", nil},
		{"CR-over-limit", codexRPCFrameLimit, "\r\n", errCodexRPCSize},
		{"limit-without-LF", codexRPCFrameLimit, "", io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const envelope = `{"method":"m","params":""}`
			wire := `{"method":"m","params":"` + strings.Repeat("a", tc.size-len(envelope)) + `"}` + tc.ending
			if tc.ending != "" {
				wire += codexRPCStreamCallback
			}
			source := &codexRPCStreamCountingReader{tail: strings.NewReader(wire)}
			r := newCodexRPCReader(source)
			message, err := r.Next()
			if err != tc.want {
				t.Fatalf("boundary error = %v, want %v", err, tc.want)
			}
			if len(r.frame) > codexRPCFrameLimit || cap(r.frame) > codexRPCFrameLimit {
				t.Fatalf("retained frame exceeded limit: len=%d cap=%d", len(r.frame), cap(r.frame))
			}
			if tc.want != nil {
				if !reflect.DeepEqual(message, codexRPCMessage{}) {
					t.Fatal("invalid boundary returned a partial message")
				}
				codexRPCStreamTerminal(t, r, source, tc.want)
				return
			}
			if message.Method == nil || *message.Method != "m" || len(message.Params) != tc.size-len(envelope)+2 {
				t.Fatal("exact-limit frame changed")
			}
			if next, err := r.Next(); err != nil || string(next.ID) != `"rpc-next"` {
				t.Fatalf("next complete frame lost: %v", err)
			}
			if _, err := r.Next(); err != io.EOF {
				t.Fatalf("expected clean EOF: %v", err)
			}
			codexRPCStreamTerminal(t, r, source, io.EOF)
		})
	}
}

// Return the final data and error together, including repeated (0, nil).
type codexRPCStreamResultReader struct {
	data string
	err  error
}

func (r *codexRPCStreamResultReader) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) > 0 {
		return n, nil
	}
	return n, r.err
}

func TestCodexRPCStreamReadErrors(t *testing.T) {
	private := errors.New("PRIVATE-PAYLOAD: reader failure")
	prefix := "{\"method\":\"one\"}\n{\"method\":\"two\"}\n"
	tail := `{"method":"tail"}`
	for _, tc := range []struct {
		name, data string
		sourceErr  error
		methods    []string
		want       error // nil means an arbitrary error must be sanitized.
	}{
		{"empty", "", io.EOF, nil, io.EOF},
		{"complete-prefix-EOF", prefix, io.EOF, []string{"one", "two"}, io.EOF},
		{"prefix-and-tail-EOF", prefix + tail, io.EOF, []string{"one", "two"}, io.ErrUnexpectedEOF},
		{"valid-without-LF", tail, io.EOF, nil, io.ErrUnexpectedEOF},
		{"partial-JSON", `{"method":`, io.EOF, nil, io.ErrUnexpectedEOF},
		{"whitespace-without-LF", " \r", io.EOF, nil, io.ErrUnexpectedEOF},
		{"arbitrary-error", "", private, nil, nil},
		{"complete-prefix-error", prefix, private, []string{"one", "two"}, nil},
		{"prefix-and-tail-error", prefix + tail, private, []string{"one", "two"}, nil},
		{"unexpected-EOF", tail, io.ErrUnexpectedEOF, nil, io.ErrUnexpectedEOF},
		{"complete-prefix-unexpected-EOF", prefix, io.ErrUnexpectedEOF, []string{"one", "two"}, io.ErrUnexpectedEOF},
		{"no-progress", "", nil, nil, io.ErrNoProgress},
		{"explicit-no-progress", "", io.ErrNoProgress, nil, io.ErrNoProgress},
		{"complete-prefix-no-progress", prefix, io.ErrNoProgress, []string{"one", "two"}, io.ErrNoProgress},
		{"wrapped-EOF", "", fmt.Errorf("PRIVATE-PAYLOAD: %w", io.EOF), nil, io.EOF},
		{"wrapped-EOF-with-tail", tail, fmt.Errorf("PRIVATE-PAYLOAD: %w", io.EOF), nil, io.ErrUnexpectedEOF},
		{"wrapped-unexpected-EOF", "", fmt.Errorf("PRIVATE-PAYLOAD: %w", io.ErrUnexpectedEOF), nil, io.ErrUnexpectedEOF},
		{"wrapped-no-progress", prefix, fmt.Errorf("PRIVATE-PAYLOAD: %w", io.ErrNoProgress), []string{"one", "two"}, io.ErrNoProgress},
		{"source-buffer-full", "", bufio.ErrBufferFull, nil, nil},
		{"prefix-source-buffer-full", prefix, bufio.ErrBufferFull, []string{"one", "two"}, nil},
		{"prefix-and-tail-source-buffer-full", prefix + tail, bufio.ErrBufferFull, []string{"one", "two"}, nil},
		{"source-frame-sentinel", "", errCodexRPCFrame, nil, nil},
		{"source-size-sentinel", "", errCodexRPCSize, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &codexRPCStreamCountingReader{tail: &codexRPCStreamResultReader{data: tc.data, err: tc.sourceErr}}
			r := newCodexRPCReader(source)
			for _, method := range tc.methods {
				message, err := r.Next()
				if err != nil || message.Method == nil || *message.Method != method {
					t.Fatalf("complete prefix lost or reordered: want %s, error %v", method, err)
				}
			}
			message, err := r.Next()
			if err == nil || tc.want != nil && err != tc.want || !reflect.DeepEqual(message, codexRPCMessage{}) {
				t.Fatalf("terminal message/error = %+v / %v, want %v", message, err, tc.want)
			}
			if strings.Contains(err.Error(), "PRIVATE-PAYLOAD") || errors.Is(err, private) || tc.want == nil && err != errCodexRPCRead {
				t.Fatal("underlying reader error escaped sanitization")
			}
			if source.reads > 100 || tc.sourceErr != nil && source.reads != 1 {
				t.Fatalf("read again after a pending error or failed to bound empty reads: %d", source.reads)
			}
			codexRPCStreamTerminal(t, r, source, err)
		})
	}
}

func TestCodexRPCStreamMalformedSticky(t *testing.T) {
	for _, tc := range []struct{ name, frame string }{
		{"empty", ""},
		{"whitespace", " \t\r"},
		{"malformed", `{"method":"PRIVATE-PAYLOAD"`},
		{"invalid-UTF8", "{\"method\":\"PRIVATE-PAYLOAD\xff\"}"},
		{"lone-surrogate", `{"method":"\uD800"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &codexRPCStreamCountingReader{tail: iotest.OneByteReader(strings.NewReader(tc.frame + "\n" + codexRPCStreamCallback))}
			r := newCodexRPCReader(source)
			message, err := r.Next()
			if err != errCodexRPCFrame || !reflect.DeepEqual(message, codexRPCMessage{}) {
				t.Fatalf("malformed prefix returned message or wrong error: %v", err)
			}
			codexRPCStreamTerminal(t, r, source, errCodexRPCFrame)
		})
	}
}

type codexRPCStreamReaderFunc func([]byte) (int, error)

func (f codexRPCStreamReaderFunc) Read(p []byte) (int, error) { return f(p) }

func TestCodexRPCStreamPipeClose(t *testing.T) {
	pipe, writer := io.Pipe()
	defer pipe.Close()
	defer writer.Close()
	started := make(chan struct{}, 1)
	source := &codexRPCStreamCountingReader{tail: codexRPCStreamReaderFunc(func(p []byte) (int, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		return pipe.Read(p)
	})}
	r := newCodexRPCReader(source)
	type result struct {
		message codexRPCMessage
		err     error
	}
	done := make(chan result, 1)
	go func() {
		message, err := r.Next()
		done <- result{message, err}
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	select {
	case <-started:
	case <-deadline.C:
		t.Fatal("Next did not enter the pipe read")
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != io.EOF || !reflect.DeepEqual(got.message, codexRPCMessage{}) {
			t.Fatalf("pipe close returned a message or wrong error: %v", got.err)
		}
	case <-deadline.C:
		t.Fatal("Next did not finish after the test closed the writer")
	}
	codexRPCStreamTerminal(t, r, source, io.EOF)
}
