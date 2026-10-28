package text

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/go-text/typesetting/font"
	ot "github.com/go-text/typesetting/font/opentype"
)

// Loading system fonts. go-text's font.NewFont reads every table into
// memory, which is fine for text faces (a few MB) but not for Apple Color
// Emoji, whose sbix bitmaps are 188 MB. So the sbix table is hidden from
// go-text (its tag is rewritten as the table directory is read) and read on
// demand instead, glyph by glyph, with ReadAt.

var sbixTag = [4]byte{'s', 'b', 'i', 'x'}

// tableRecord is one entry of a face's table directory.
type tableRecord struct {
	tagAt       int64 // file offset of the record's tag
	offset, len uint32
}

// faceTables reads the table directory of face index in a font file (a
// single sfnt or a TrueType collection) and returns the sbix record, if any.
func faceTables(r io.ReaderAt, index int) (sbix tableRecord, found bool, err error) {
	var hdr [12]byte
	if _, err := r.ReadAt(hdr[:], 0); err != nil {
		return sbix, false, err
	}
	var base int64
	if string(hdr[:4]) == "ttcf" {
		n := int(binary.BigEndian.Uint32(hdr[8:]))
		if index >= n {
			return sbix, false, fmt.Errorf("face %d out of %d", index, n)
		}
		var off [4]byte
		if _, err := r.ReadAt(off[:], 12+4*int64(index)); err != nil {
			return sbix, false, err
		}
		base = int64(binary.BigEndian.Uint32(off[:]))
		if _, err := r.ReadAt(hdr[:], base); err != nil {
			return sbix, false, err
		}
	} else if index != 0 {
		return sbix, false, fmt.Errorf("face %d of a single-face file", index)
	}
	n := int(binary.BigEndian.Uint16(hdr[4:]))
	recs := make([]byte, 16*n)
	if _, err := r.ReadAt(recs, base+12); err != nil {
		return sbix, false, err
	}
	for i := range n {
		rec := recs[16*i:]
		if [4]byte(rec[:4]) == sbixTag {
			return tableRecord{
				tagAt:  base + 12 + 16*int64(i),
				offset: binary.BigEndian.Uint32(rec[8:]),
				len:    binary.BigEndian.Uint32(rec[12:]),
			}, true, nil
		}
	}
	return sbix, false, nil
}

// hiddenTable is a font.Resource over a file that renames one table's tag
// (to "xbis") wherever it is read, so go-text's loader never sees it.
type hiddenTable struct {
	f     *os.File
	pos   int64
	tagAt int64
}

func (h *hiddenTable) mask(p []byte, off int64) {
	for k := range int64(4) {
		if i := h.tagAt + k - off; i >= 0 && i < int64(len(p)) {
			p[i] = "xbis"[k]
		}
	}
}

func (h *hiddenTable) ReadAt(p []byte, off int64) (int, error) {
	n, err := h.f.ReadAt(p, off)
	h.mask(p[:n], off)
	return n, err
}

func (h *hiddenTable) Read(p []byte) (int, error) {
	n, err := h.ReadAt(p, h.pos)
	h.pos += int64(n)
	if err == io.EOF && n > 0 {
		err = nil
	}
	return n, err
}

func (h *hiddenTable) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		h.pos = off
	case io.SeekCurrent:
		h.pos += off
	case io.SeekEnd:
		fi, err := h.f.Stat()
		if err != nil {
			return 0, err
		}
		h.pos = fi.Size() + off
	}
	if h.pos < 0 {
		return 0, errors.New("seek before start")
	}
	return h.pos, nil
}

// loadFace parses face loc.Index of loc.File. A face with an sbix table
// comes back with the table left on disk: the returned sbix reads it on
// demand and keeps the file open for the life of the process.
func loadFace(loc font.FontID) (*font.Font, *sbixTable, error) {
	f, err := os.Open(loc.File)
	if err != nil {
		return nil, nil, err
	}
	rec, hasSbix, err := faceTables(f, int(loc.Index))
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("%s: %w", loc.File, err)
	}
	var res ot.Resource = f
	if hasSbix {
		res = &hiddenTable{f: f, tagAt: rec.tagAt}
	}
	lds, err := ot.NewLoaders(res)
	if err != nil || int(loc.Index) >= len(lds) {
		f.Close()
		return nil, nil, fmt.Errorf("%s: face %d: %v", loc.File, loc.Index, err)
	}
	ft, err := font.NewFont(lds[loc.Index])
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("%s: face %d: %w", loc.File, loc.Index, err)
	}
	if !hasSbix {
		// NewFont copied every table it uses.
		f.Close()
		return ft, nil, nil
	}
	maxp, err := lds[loc.Index].RawTable(ot.MustNewTag("maxp"))
	if err != nil || len(maxp) < 6 {
		f.Close()
		return nil, nil, fmt.Errorf("%s: face %d: no maxp", loc.File, loc.Index)
	}
	sb := &sbixTable{r: f, off: int64(rec.offset), len: int64(rec.len), glyphs: int(binary.BigEndian.Uint16(maxp[4:]))}
	return ft, sb, nil
}
