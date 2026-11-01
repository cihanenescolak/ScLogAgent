package prefilter

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"unicode/utf8"
)

// MaxLineBytes caps a single line; anything beyond is dropped (never seen in
// real logs, protects memory against corrupt files).
const MaxLineBytes = 32 << 20

var bom = []byte{0xEF, 0xBB, 0xBF}

// DecodeLine turns raw line bytes (without "\n") into the text the protocol
// hashes: one trailing "\r" removed, a leading UTF-8 BOM removed on the first
// line, invalid UTF-8 replaced by U+FFFD the way the browser's TextDecoder
// does it (WHATWG "maximal subpart" rule), so browser and agent hash alike.
func DecodeLine(raw []byte, first bool) string {
	if first {
		raw = bytes.TrimPrefix(raw, bom)
	}
	if n := len(raw); n > 0 && raw[n-1] == '\r' {
		raw = raw[:n-1]
	}
	if utf8.Valid(raw) {
		return string(raw)
	}
	return replaceInvalid(raw)
}

func replaceInvalid(b []byte) string {
	out := make([]byte, 0, len(b)+8)
	const repl = "�"
	i := 0
	for i < len(b) {
		c := b[i]
		if c < 0x80 {
			out = append(out, c)
			i++
			continue
		}
		need, lo, hi := 0, byte(0x80), byte(0xBF)
		switch {
		case c >= 0xC2 && c <= 0xDF:
			need = 1
		case c == 0xE0:
			need, lo = 2, 0xA0
		case c >= 0xE1 && c <= 0xEC, c == 0xEE, c == 0xEF:
			need = 2
		case c == 0xED:
			need, hi = 2, 0x9F
		case c == 0xF0:
			need, lo = 3, 0x90
		case c >= 0xF1 && c <= 0xF3:
			need = 3
		case c == 0xF4:
			need, hi = 3, 0x8F
		default:
			out = append(out, repl...)
			i++
			continue
		}
		j := i + 1
		valid := true
		for k := 0; k < need; k++ {
			if j >= len(b) || b[j] < lo || b[j] > hi {
				valid = false
				break
			}
			lo, hi = 0x80, 0xBF
			j++
		}
		if valid {
			out = append(out, b[i:j]...)
		} else {
			out = append(out, repl...)
		}
		i = j
	}
	return string(out)
}

// LineReader splits a stream on "\n" without limiting line length.
type LineReader struct {
	br     *bufio.Reader
	lineNo int
	offset int64
	buf    []byte
	done   bool
}

func NewLineReader(r io.Reader) *LineReader {
	return &LineReader{br: bufio.NewReaderSize(r, 256<<10)}
}

// Next returns the next line. ok=false at end of input.
func (l *LineReader) Next() (text string, ok bool, err error) {
	if l.done {
		return "", false, nil
	}
	l.buf = l.buf[:0]
	truncated := false
	for {
		chunk, err := l.br.ReadSlice('\n')
		l.offset += int64(len(chunk))
		if !truncated {
			if len(l.buf)+len(chunk) > MaxLineBytes {
				room := MaxLineBytes - len(l.buf)
				l.buf = append(l.buf, chunk[:max(room, 0)]...)
				truncated = true
			} else {
				l.buf = append(l.buf, chunk...)
			}
		}
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			l.done = true
			if len(l.buf) == 0 {
				return "", false, nil
			}
			break
		}
		return "", false, err
	}
	raw := l.buf
	if n := len(raw); n > 0 && raw[n-1] == '\n' {
		raw = raw[:n-1]
	}
	l.lineNo++
	return DecodeLine(raw, l.lineNo == 1), true, nil
}

// LineNo is the 1-based number of the last line returned.
func (l *LineReader) LineNo() int { return l.lineNo }

// Offset is the number of bytes consumed.
func (l *LineReader) Offset() int64 { return l.offset }
