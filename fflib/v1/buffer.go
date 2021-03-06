// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package v1

// Simple byte buffer for marshaling data.

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"unicode/utf8"
)

type grower interface {
	Grow(n int)
}

type truncater interface {
	Truncate(n int)
	Reset()
}

type bytesReader interface {
	Bytes() []byte
	String() string
}

type runeWriter interface {
	WriteRune(r rune) (n int, err error)
}

type stringWriter interface {
	WriteString(s string) (n int, err error)
}

type lener interface {
	Len() int
}

type rewinder interface {
	Rewind(n int) (err error)
}

// TODO(pquerna): continue to reduce these interfaces

type EncodingBuffer interface {
	io.Writer
	io.WriterTo
	io.ByteWriter
	stringWriter
	truncater
	grower
	rewinder
}

type DecodingBuffer interface {
	io.ReadWriter
	io.ByteWriter
	stringWriter
	runeWriter
	truncater
	grower
	bytesReader
	lener
}

var pools [14]sync.Pool
var pool64 *sync.Pool

func init() {
	var i uint
	// TODO(pquerna): add science here around actual pool sizes.
	for i = 6; i < 20; i++ {
		n := 1 << i
		pools[poolNum(n)].New = func() interface{} { return make([]byte, 0, n) }
	}
	pool64 = &pools[0]
}

func poolNum(i int) int {
	// TODO(pquerna): convert to log2 w/ bsr asm instruction:
	// 	<https://groups.google.com/forum/#!topic/golang-nuts/uAb5J1_y7ns>
	if i <= 64 {
		return 0
	} else if i <= 128 {
		return 1
	} else if i <= 256 {
		return 2
	} else if i <= 512 {
		return 3
	} else if i <= 1024 {
		return 4
	} else if i <= 2048 {
		return 5
	} else if i <= 4096 {
		return 6
	} else if i <= 8192 {
		return 7
	} else if i <= 16384 {
		return 8
	} else if i <= 32768 {
		return 9
	} else if i <= 65536 {
		return 10
	} else if i <= 131072 {
		return 11
	} else if i <= 262144 {
		return 12
	} else if i <= 524288 {
		return 13
	} else {
		return -1
	}
}

// Send a buffer to the Pool to reuse for other instances.
// You may no longer utilize the content of the buffer, since it may be used
// by other goroutines.
func Pool(b []byte) {
	c := cap(b)
	pn := poolNum(c)
	if pn != -1 {
		pools[pn].Put(b[0:0])
	}
	// if we didn't have a slot for this []byte, we just drop it and let the GC
	// take care of it.
}

// A Buffer is a variable-sized buffer of bytes with Read and Write methods.
// The zero value for Buffer is an empty buffer ready to use.
type Buffer struct {
	buf       []byte            // contents are the bytes buf[off : len(buf)]
	off       int               // read at &buf[off], write at &buf[len(buf)]
	runeBytes [utf8.UTFMax]byte // avoid allocation of slice on each WriteByte or Rune
}

// ErrTooLarge is passed to panic if memory cannot be allocated to store data in a buffer.
var ErrTooLarge = errors.New("fflib.v1.Buffer: too large")

// Bytes returns a slice of the contents of the unread portion of the buffer;
// len(b.Bytes()) == b.Len().  If the caller changes the contents of the
// returned slice, the contents of the buffer will change provided there
// are no intervening method calls on the Buffer.
func (b *Buffer) Bytes() []byte { return b.buf[b.off:] }

// String returns the contents of the unread portion of the buffer
// as a string.  If the Buffer is a nil pointer, it returns "<nil>".
func (b *Buffer) String() string {
	if b == nil {
		// Special case, useful in debugging.
		return "<nil>"
	}
	return string(b.buf[b.off:])
}

// Len returns the number of bytes of the unread portion of the buffer;
// b.Len() == len(b.Bytes()).
func (b *Buffer) Len() int { return len(b.buf) - b.off }

// Truncate discards all but the first n unread bytes from the buffer.
// It panics if n is negative or greater than the length of the buffer.
func (b *Buffer) Truncate(n int) {
	if n == 0 {
		b.off = 0
		b.buf = b.buf[0:0]
	} else {
		b.buf = b.buf[0 : b.off+n]
	}
}

// Reset resets the buffer so it has no content.
// b.Reset() is the same as b.Truncate(0).
func (b *Buffer) Reset() { b.Truncate(0) }

// grow grows the buffer to guarantee space for n more bytes.
// It returns the index where bytes should be written.
// If the buffer can't grow it will panic with ErrTooLarge.
func (b *Buffer) grow(n int) int {
	// If we have no buffer, get one from the pool
	m := b.Len()
	if m == 0 {
		if b.buf == nil {
			b.buf = makeSlice(2 * n)
			b.off = 0
		} else if b.off != 0 {
			// If buffer is empty, reset to recover space.
			b.Truncate(0)
		}
	}
	if len(b.buf)+n > cap(b.buf) {
		var buf []byte
		if m+n <= cap(b.buf)/2 {
			// We can slide things down instead of allocating a new
			// slice. We only need m+n <= cap(b.buf) to slide, but
			// we instead let capacity get twice as large so we
			// don't spend all our time copying.
			copy(b.buf[:], b.buf[b.off:])
			buf = b.buf[:m]
		} else {
			// not enough space anywhere
			buf = makeSlice(2*cap(b.buf) + n)
			copy(buf, b.buf[b.off:])
		}
		Pool(b.buf)
		b.buf = buf
		b.off = 0
	}
	b.buf = b.buf[0 : b.off+m+n]
	return b.off + m
}

// Grow grows the buffer's capacity, if necessary, to guarantee space for
// another n bytes. After Grow(n), at least n bytes can be written to the
// buffer without another allocation.
// If n is negative, Grow will panic.
// If the buffer can't grow it will panic with ErrTooLarge.
func (b *Buffer) Grow(n int) {
	if n < 0 {
		panic("bytes.Buffer.Grow: negative count")
	}
	m := b.grow(n)
	b.buf = b.buf[0:m]
}

// Write appends the contents of p to the buffer, growing the buffer as
// needed. The return value n is the length of p; err is always nil. If the
// buffer becomes too large, Write will panic with ErrTooLarge.
func (b *Buffer) Write(p []byte) (n int, err error) {
	m := b.grow(len(p))
	return copy(b.buf[m:], p), nil
}

// WriteString appends the contents of s to the buffer, growing the buffer as
// needed. The return value n is the length of s; err is always nil. If the
// buffer becomes too large, WriteString will panic with ErrTooLarge.
func (b *Buffer) WriteString(s string) (n int, err error) {
	m := b.grow(len(s))
	return copy(b.buf[m:], s), nil
}

// MinRead is the minimum slice size passed to a Read call by
// Buffer.ReadFrom.  As long as the Buffer has at least MinRead bytes beyond
// what is required to hold the contents of r, ReadFrom will not grow the
// underlying buffer.
const minRead = 512

// ReadFrom reads data from r until EOF and appends it to the buffer, growing
// the buffer as needed. The return value n is the number of bytes read. Any
// error except io.EOF encountered during the read is also returned. If the
// buffer becomes too large, ReadFrom will panic with ErrTooLarge.
func (b *Buffer) ReadFrom(r io.Reader) (n int64, err error) {
	// If buffer is empty, reset to recover space.
	if b.off >= len(b.buf) {
		b.Truncate(0)
	}
	for {
		if free := cap(b.buf) - len(b.buf); free < minRead {
			// not enough space at end
			newBuf := b.buf
			if b.off+free < minRead {
				// not enough space using beginning of buffer;
				// double buffer capacity
				newBuf = makeSlice(2*cap(b.buf) + minRead)
			}
			copy(newBuf, b.buf[b.off:])
			Pool(b.buf)
			b.buf = newBuf[:len(b.buf)-b.off]
			b.off = 0
		}
		m, e := r.Read(b.buf[len(b.buf):cap(b.buf)])
		b.buf = b.buf[0 : len(b.buf)+m]
		n += int64(m)
		if e == io.EOF {
			break
		}
		if e != nil {
			return n, e
		}
	}
	return n, nil // err is EOF, so return nil explicitly
}

// makeSlice allocates a slice of size n -- it will attempt to use a pool'ed
// instance whenever possible.
func makeSlice(n int) []byte {
	if n <= 64 {
		return pool64.Get().([]byte)[0:n]
	}

	pn := poolNum(n)

	if pn != -1 {
		return pools[pn].Get().([]byte)[0:n]
	} else {
		return make([]byte, n)
	}
}

// WriteTo writes data to w until the buffer is drained or an error occurs.
// The return value n is the number of bytes written; it always fits into an
// int, but it is int64 to match the io.WriterTo interface. Any error
// encountered during the write is also returned.
func (b *Buffer) WriteTo(w io.Writer) (n int64, err error) {
	if b.off < len(b.buf) {
		nBytes := b.Len()
		m, e := w.Write(b.buf[b.off:])
		if m > nBytes {
			panic("bytes.Buffer.WriteTo: invalid Write count")
		}
		b.off += m
		n = int64(m)
		if e != nil {
			return n, e
		}
		// all bytes should have been written, by definition of
		// Write method in io.Writer
		if m != nBytes {
			return n, io.ErrShortWrite
		}
	}
	// Buffer is now empty; reset.
	b.Truncate(0)
	return
}

// WriteByte appends the byte c to the buffer, growing the buffer as needed.
// The returned error is always nil, but is included to match bufio.Writer's
// WriteByte. If the buffer becomes too large, WriteByte will panic with
// ErrTooLarge.
func (b *Buffer) WriteByte(c byte) error {
	m := b.grow(1)
	b.buf[m] = c
	return nil
}

func (b *Buffer) Rewind(n int) error {
	b.buf = b.buf[:len(b.buf)-n]
	return nil
}

// WriteRune appends the UTF-8 encoding of Unicode code point r to the
// buffer, returning its length and an error, which is always nil but is
// included to match bufio.Writer's WriteRune. The buffer is grown as needed;
// if it becomes too large, WriteRune will panic with ErrTooLarge.
func (b *Buffer) WriteRune(r rune) (n int, err error) {
	if r < utf8.RuneSelf {
		b.WriteByte(byte(r))
		return 1, nil
	}
	n = utf8.EncodeRune(b.runeBytes[0:], r)
	b.Write(b.runeBytes[0:n])
	return n, nil
}

// Read reads the next len(p) bytes from the buffer or until the buffer
// is drained.  The return value n is the number of bytes read.  If the
// buffer has no data to return, err is io.EOF (unless len(p) is zero);
// otherwise it is nil.
func (b *Buffer) Read(p []byte) (n int, err error) {
	if b.off >= len(b.buf) {
		// Buffer is empty, reset to recover space.
		b.Truncate(0)
		if len(p) == 0 {
			return
		}
		return 0, io.EOF
	}
	n = copy(p, b.buf[b.off:])
	b.off += n
	return
}

// Next returns a slice containing the next n bytes from the buffer,
// advancing the buffer as if the bytes had been returned by Read.
// If there are fewer than n bytes in the buffer, Next returns the entire buffer.
// The slice is only valid until the next call to a read or write method.
func (b *Buffer) Next(n int) []byte {
	m := b.Len()
	if n > m {
		n = m
	}
	data := b.buf[b.off : b.off+n]
	b.off += n
	return data
}

// ReadByte reads and returns the next byte from the buffer.
// If no byte is available, it returns error io.EOF.
func (b *Buffer) ReadByte() (c byte, err error) {
	if b.off >= len(b.buf) {
		// Buffer is empty, reset to recover space.
		b.Truncate(0)
		return 0, io.EOF
	}
	c = b.buf[b.off]
	b.off++
	return c, nil
}

// ReadRune reads and returns the next UTF-8-encoded
// Unicode code point from the buffer.
// If no bytes are available, the error returned is io.EOF.
// If the bytes are an erroneous UTF-8 encoding, it
// consumes one byte and returns U+FFFD, 1.
func (b *Buffer) ReadRune() (r rune, size int, err error) {
	if b.off >= len(b.buf) {
		// Buffer is empty, reset to recover space.
		b.Truncate(0)
		return 0, 0, io.EOF
	}
	c := b.buf[b.off]
	if c < utf8.RuneSelf {
		b.off++
		return rune(c), 1, nil
	}
	r, n := utf8.DecodeRune(b.buf[b.off:])
	b.off += n
	return r, n, nil
}

// ReadBytes reads until the first occurrence of delim in the input,
// returning a slice containing the data up to and including the delimiter.
// If ReadBytes encounters an error before finding a delimiter,
// it returns the data read before the error and the error itself (often io.EOF).
// ReadBytes returns err != nil if and only if the returned data does not end in
// delim.
func (b *Buffer) ReadBytes(delim byte) (line []byte, err error) {
	slice, err := b.readSlice(delim)
	// return a copy of slice. The buffer's backing array may
	// be overwritten by later calls.
	line = append(line, slice...)
	return
}

// readSlice is like ReadBytes but returns a reference to internal buffer data.
func (b *Buffer) readSlice(delim byte) (line []byte, err error) {
	i := bytes.IndexByte(b.buf[b.off:], delim)
	end := b.off + i + 1
	if i < 0 {
		end = len(b.buf)
		err = io.EOF
	}
	line = b.buf[b.off:end]
	b.off = end
	return line, err
}

// ReadString reads until the first occurrence of delim in the input,
// returning a string containing the data up to and including the delimiter.
// If ReadString encounters an error before finding a delimiter,
// it returns the data read before the error and the error itself (often io.EOF).
// ReadString returns err != nil if and only if the returned data does not end
// in delim.
func (b *Buffer) ReadString(delim byte) (line string, err error) {
	slice, err := b.readSlice(delim)
	return string(slice), err
}

// NewBuffer creates and initializes a new Buffer using buf as its initial
// contents.  It is intended to prepare a Buffer to read existing data.  It
// can also be used to size the internal buffer for writing. To do that,
// buf should have the desired capacity but a length of zero.
//
// In most cases, new(Buffer) (or just declaring a Buffer variable) is
// sufficient to initialize a Buffer.
func NewBuffer(buf []byte) *Buffer { return &Buffer{buf: buf} }

// NewBufferString creates and initializes a new Buffer using string s as its
// initial contents. It is intended to prepare a buffer to read an existing
// string.
//
// In most cases, new(Buffer) (or just declaring a Buffer variable) is
// sufficient to initialize a Buffer.
func NewBufferString(s string) *Buffer {
	return &Buffer{buf: []byte(s)}
}
