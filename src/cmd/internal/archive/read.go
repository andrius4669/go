// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package archive implements reading of archive files generated by the Go
// toolchain.
package archive

import (
	"bufio"
	"bytes"
	"cmd/internal/bio"
	"cmd/internal/goobj2"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
)

// A Data is a reference to data stored in an object file.
// It records the offset and size of the data, so that a client can
// read the data only if necessary.
type Data struct {
	Offset int64
	Size   int64
}

type Archive struct {
	f       *os.File
	Entries []Entry
}

type Entry struct {
	Name string
	Type EntryType
	Data
	Obj *GoObj // nil if this entry is not a Go object file
}

type EntryType int

const (
	EntryPkgDef EntryType = iota
	EntryGoObj
	EntryNativeObj
)

type GoObj struct {
	TextHeader []byte
	Data
}

var (
	archiveHeader = []byte("!<arch>\n")
	archiveMagic  = []byte("`\n")
	goobjHeader   = []byte("go objec") // truncated to size of archiveHeader

	errCorruptArchive   = errors.New("corrupt archive")
	errTruncatedArchive = errors.New("truncated archive")
	errCorruptObject    = errors.New("corrupt object file")
	errNotObject        = errors.New("unrecognized object file format")
)

// An objReader is an object file reader.
type objReader struct {
	a      *Archive
	b      *bio.Reader
	err    error
	offset int64
	limit  int64
	tmp    [256]byte
}

func (r *objReader) init(f *os.File) {
	r.a = &Archive{f, nil}
	r.offset, _ = f.Seek(0, io.SeekCurrent)
	r.limit, _ = f.Seek(0, io.SeekEnd)
	f.Seek(r.offset, io.SeekStart)
	r.b = bio.NewReader(f)
}

// error records that an error occurred.
// It returns only the first error, so that an error
// caused by an earlier error does not discard information
// about the earlier error.
func (r *objReader) error(err error) error {
	if r.err == nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		r.err = err
	}
	// panic("corrupt") // useful for debugging
	return r.err
}

// peek returns the next n bytes without advancing the reader.
func (r *objReader) peek(n int) ([]byte, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.offset >= r.limit {
		r.error(io.ErrUnexpectedEOF)
		return nil, r.err
	}
	b, err := r.b.Peek(n)
	if err != nil {
		if err != bufio.ErrBufferFull {
			r.error(err)
		}
	}
	return b, err
}

// readByte reads and returns a byte from the input file.
// On I/O error or EOF, it records the error but returns byte 0.
// A sequence of 0 bytes will eventually terminate any
// parsing state in the object file. In particular, it ends the
// reading of a varint.
func (r *objReader) readByte() byte {
	if r.err != nil {
		return 0
	}
	if r.offset >= r.limit {
		r.error(io.ErrUnexpectedEOF)
		return 0
	}
	b, err := r.b.ReadByte()
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		r.error(err)
		b = 0
	} else {
		r.offset++
	}
	return b
}

// read reads exactly len(b) bytes from the input file.
// If an error occurs, read returns the error but also
// records it, so it is safe for callers to ignore the result
// as long as delaying the report is not a problem.
func (r *objReader) readFull(b []byte) error {
	if r.err != nil {
		return r.err
	}
	if r.offset+int64(len(b)) > r.limit {
		return r.error(io.ErrUnexpectedEOF)
	}
	n, err := io.ReadFull(r.b, b)
	r.offset += int64(n)
	if err != nil {
		return r.error(err)
	}
	return nil
}

// skip skips n bytes in the input.
func (r *objReader) skip(n int64) {
	if n < 0 {
		r.error(fmt.Errorf("debug/goobj: internal error: misuse of skip"))
	}
	if n < int64(len(r.tmp)) {
		// Since the data is so small, a just reading from the buffered
		// reader is better than flushing the buffer and seeking.
		r.readFull(r.tmp[:n])
	} else if n <= int64(r.b.Buffered()) {
		// Even though the data is not small, it has already been read.
		// Advance the buffer instead of seeking.
		for n > int64(len(r.tmp)) {
			r.readFull(r.tmp[:])
			n -= int64(len(r.tmp))
		}
		r.readFull(r.tmp[:n])
	} else {
		// Seek, giving up buffered data.
		r.b.MustSeek(r.offset+n, io.SeekStart)
		r.offset += n
	}
}

// Parse parses an object file or archive from f.
func Parse(f *os.File) (*Archive, error) {
	var r objReader
	r.init(f)
	t, err := r.peek(8)
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}

	switch {
	default:
		return nil, errNotObject

	case bytes.Equal(t, archiveHeader):
		if err := r.parseArchive(); err != nil {
			return nil, err
		}
	case bytes.Equal(t, goobjHeader):
		off := r.offset
		o := &GoObj{}
		if err := r.parseObject(o, r.limit-off); err != nil {
			return nil, err
		}
		r.a.Entries = []Entry{{f.Name(), EntryGoObj, Data{off, r.limit - off}, o}}
	}

	return r.a, nil
}

// trimSpace removes trailing spaces from b and returns the corresponding string.
// This effectively parses the form used in archive headers.
func trimSpace(b []byte) string {
	return string(bytes.TrimRight(b, " "))
}

// parseArchive parses a Unix archive of Go object files.
func (r *objReader) parseArchive() error {
	r.readFull(r.tmp[:8]) // consume header (already checked)
	for r.offset < r.limit {
		if err := r.readFull(r.tmp[:60]); err != nil {
			return err
		}
		data := r.tmp[:60]

		// Each file is preceded by this text header (slice indices in first column):
		//	 0:16	name
		//	16:28 date
		//	28:34 uid
		//	34:40 gid
		//	40:48 mode
		//	48:58 size
		//	58:60 magic - `\n
		// We only care about name, size, and magic.
		// The fields are space-padded on the right.
		// The size is in decimal.
		// The file data - size bytes - follows the header.
		// Headers are 2-byte aligned, so if size is odd, an extra padding
		// byte sits between the file data and the next header.
		// The file data that follows is padded to an even number of bytes:
		// if size is odd, an extra padding byte is inserted betw the next header.
		if len(data) < 60 {
			return errTruncatedArchive
		}
		if !bytes.Equal(data[58:60], archiveMagic) {
			return errCorruptArchive
		}
		name := trimSpace(data[0:16])
		size, err := strconv.ParseInt(trimSpace(data[48:58]), 10, 64)
		if err != nil {
			return errCorruptArchive
		}
		data = data[60:]
		fsize := size + size&1
		if fsize < 0 || fsize < size {
			return errCorruptArchive
		}
		switch name {
		case "__.PKGDEF":
			r.a.Entries = append(r.a.Entries, Entry{name, EntryPkgDef, Data{r.offset, size}, nil})
			r.skip(size)
		default:
			var typ EntryType
			var o *GoObj
			offset := r.offset
			p, err := r.peek(8)
			if err != nil {
				return err
			}
			if bytes.Equal(p, goobjHeader) {
				typ = EntryGoObj
				o = &GoObj{}
				r.parseObject(o, size)
			} else {
				typ = EntryNativeObj
				r.skip(size)
			}
			r.a.Entries = append(r.a.Entries, Entry{name, typ, Data{offset, size}, o})
		}
		if size&1 != 0 {
			r.skip(1)
		}
	}
	return nil
}

// parseObject parses a single Go object file.
// The object file consists of a textual header ending in "\n!\n"
// and then the part we want to parse begins.
// The format of that part is defined in a comment at the top
// of src/liblink/objfile.c.
func (r *objReader) parseObject(o *GoObj, size int64) error {
	h := make([]byte, 0, 256)
	var c1, c2, c3 byte
	for {
		c1, c2, c3 = c2, c3, r.readByte()
		h = append(h, c3)
		// The new export format can contain 0 bytes.
		// Don't consider them errors, only look for r.err != nil.
		if r.err != nil {
			return errCorruptObject
		}
		if c1 == '\n' && c2 == '!' && c3 == '\n' {
			break
		}
	}
	o.TextHeader = h
	o.Offset = r.offset
	o.Size = size - int64(len(h))

	p, err := r.peek(8)
	if err != nil {
		return err
	}
	if !bytes.Equal(p, []byte(goobj2.Magic)) {
		return r.error(errCorruptObject)
	}
	r.skip(o.Size)
	return nil
}