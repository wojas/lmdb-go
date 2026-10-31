// Structural comparison of two data files (port of rlmdb's
// interop/src/pagecmp.rs): do two environments hold the same B-trees on
// the same page numbers, byte for byte where it matters? C does not
// initialise every byte it writes (node padding, the gap between mp_lower
// and mp_upper, free space of sub-pages, the tail of an overflow run), so
// this compares a canonical form of every page reachable from the newest
// meta page: page headers (with 1.0's mp_txnid), node pointers, node
// headers, keys and data (sub-pages recursively; F_BIGDATA nodes by their
// overflow reference), P_LEAF2 keys, overflow values, and both meta pages'
// MDB_meta. The parsing is independent of internal/format on purpose.
package engine

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"strings"
)

var ne = binary.NativeEndian

type cmpFormat struct {
	name     string
	version  uint32
	hdr      int
	base     int
	evenKeys bool
	ovpage   int
	minPsize int
}

var (
	cmpV09 = &cmpFormat{"LMDB 0.9", 1, 16, 0, false, 8, 512}
	cmpV10 = &cmpFormat{"LMDB 1.0", 3, 24, 24, true, 24, 256}
)

const (
	cmpMetaSize = 136
	cmpBranch   = 0x01
	cmpLeaf     = 0x02
	cmpOverflow = 0x04
	cmpLeaf2    = 0x20
	cmpBig      = 0x01
	cmpSub      = 0x02
	cmpDup      = 0x04
	cmpInvalid  = ^uint64(0)
	cmpDBRoot   = 40
	cmpMagic    = 0xBEEFC0DE
)

var cmpMetaDBs = [2]int{24, 72}

func formatOfMeta(page []byte) *cmpFormat {
	for _, f := range []*cmpFormat{cmpV09, cmpV10} {
		if len(page) >= f.hdr+8 && ne.Uint32(page[f.hdr:]) == cmpMagic && ne.Uint32(page[f.hdr+4:]) == f.version {
			return f
		}
	}
	return nil
}

func get(b []byte, off, n int) ([]byte, error) {
	if off < 0 || n < 0 || off+n > len(b) {
		return nil, fmt.Errorf("%d bytes at %d out of range", n, off)
	}
	return b[off : off+n], nil
}

func u16(b []byte, off int) (int, error) {
	s, err := get(b, off, 2)
	if err != nil {
		return 0, err
	}
	return int(ne.Uint16(s)), nil
}

func u64(b []byte, off int) (uint64, error) {
	s, err := get(b, off, 8)
	if err != nil {
		return 0, err
	}
	return ne.Uint64(s), nil
}

type image struct {
	data  []byte
	f     *cmpFormat
	psize int
	pages map[uint64][]byte
}

func loadImage(path string) (*image, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f := formatOfMeta(data)
	if f == nil {
		return nil, fmt.Errorf("%s: not an LMDB 0.9 or 1.0 data file", path)
	}
	ps, err := get(data, f.hdr+cmpMetaDBs[0], 4)
	if err != nil {
		return nil, err
	}
	img := &image{data: data, f: f, psize: int(ne.Uint32(ps)), pages: map[uint64][]byte{}}
	if img.psize < f.minPsize {
		return nil, fmt.Errorf("%s: page size %d", path, img.psize)
	}
	m, err := img.newestMeta()
	if err != nil {
		return nil, err
	}
	for _, db := range cmpMetaDBs {
		root, err := u64(data, m*img.psize+f.hdr+db+cmpDBRoot)
		if err != nil {
			return nil, err
		}
		if err := img.walk(root); err != nil {
			return nil, err
		}
	}
	return img, nil
}

func (img *image) newestMeta() (int, error) {
	t0, err := u64(img.data, img.f.hdr+128)
	if err != nil {
		return 0, err
	}
	t1, err := u64(img.data, img.psize+img.f.hdr+128)
	if err != nil {
		return 0, err
	}
	if t1 > t0 {
		return 1, nil
	}
	return 0, nil
}

func (img *image) metas() ([]byte, error) {
	a, err := get(img.data, img.f.hdr, cmpMetaSize)
	if err != nil {
		return nil, err
	}
	b, err := get(img.data, img.psize+img.f.hdr, cmpMetaSize)
	if err != nil {
		return nil, err
	}
	return append(append([]byte(nil), a...), b...), nil
}

func (img *image) walk(pgno uint64) error {
	if pgno == cmpInvalid {
		return nil
	}
	if _, ok := img.pages[pgno]; ok {
		return nil
	}
	page, err := get(img.data, int(pgno)*img.psize, img.psize)
	if err != nil || pgno > 1<<40 {
		return fmt.Errorf("page %d out of range", pgno)
	}
	flags, _ := u16(page, img.f.hdr-6)
	if flags&(cmpBranch|cmpLeaf) == 0 {
		return fmt.Errorf("page %d has flags %#x, not a tree page", pgno, flags)
	}
	var children []uint64
	var big [][2]uint64
	canon, err := canonical(img.f, page, &children, &big)
	if err != nil {
		return fmt.Errorf("page %d: %w", pgno, err)
	}
	img.pages[pgno] = canon
	for _, b := range big {
		start := int(b[0]) * img.psize
		h, err := get(img.data, start, img.f.hdr)
		if err != nil {
			return err
		}
		if fl, _ := u16(h, img.f.hdr-6); fl&cmpOverflow == 0 {
			return fmt.Errorf("page %d is not an overflow page", b[0])
		}
		v, err := get(img.data, start+img.f.hdr, int(b[1]))
		if err != nil {
			return err
		}
		img.pages[b[0]] = append(append([]byte(nil), h...), v...)
	}
	for _, c := range children {
		if err := img.walk(c); err != nil {
			return err
		}
	}
	return nil
}

func canonical(f *cmpFormat, page []byte, children *[]uint64, big *[][2]uint64) ([]byte, error) {
	hdr := f.hdr
	flags, err := u16(page, hdr-6)
	if err != nil {
		return nil, err
	}
	lower, err := u16(page, hdr-4)
	if err != nil {
		return nil, err
	}
	h, err := get(page, 0, hdr)
	if err != nil {
		return nil, err
	}
	out := append([]byte(nil), h...)
	empty := hdr - f.base
	if lower < empty {
		return nil, fmt.Errorf("mp_lower %d below the header", lower)
	}
	nkeys := (lower - empty) / 2
	if flags&cmpLeaf2 != 0 {
		ksize, _ := u16(page, hdr-8)
		k, err := get(page, hdr, nkeys*ksize)
		if err != nil {
			return nil, err
		}
		return append(out, k...), nil
	}
	ptrs, err := get(page, hdr, lower-empty)
	if err != nil {
		return nil, err
	}
	out = append(out, ptrs...)
	for i := 0; i < nkeys; i++ {
		p, _ := u16(page, hdr+2*i)
		ptr := p + f.base
		lo, err1 := u16(page, ptr)
		hi, err2 := u16(page, ptr+2)
		nflags, err3 := u16(page, ptr+4)
		ksize, err4 := u16(page, ptr+6)
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
			return nil, fmt.Errorf("node %d out of range", i)
		}
		nk, err := get(page, ptr, 8+ksize)
		if err != nil {
			return nil, err
		}
		out = append(out, nk...)
		if flags&cmpLeaf == 0 {
			*children = append(*children, uint64(lo)|uint64(hi)<<16|uint64(nflags)<<32)
			continue
		}
		dsize := lo | hi<<16
		klen := ksize
		if f.evenKeys {
			klen = (ksize + 1) &^ 1
		}
		dstart := ptr + 8 + klen
		if nflags&cmpBig != 0 {
			ov, err := get(page, dstart, f.ovpage)
			if err != nil {
				return nil, err
			}
			out = append(out, ov...)
			*big = append(*big, [2]uint64{ne.Uint64(ov), uint64(dsize)})
			continue
		}
		data, err := get(page, dstart, dsize)
		if err != nil {
			return nil, err
		}
		switch {
		case nflags&cmpSub != 0:
			root, err := u64(data, cmpDBRoot)
			if err != nil {
				return nil, err
			}
			*children = append(*children, root)
			out = append(out, data...)
		case nflags&cmpDup != 0:
			c, err := canonical(f, data, children, big)
			if err != nil {
				return nil, err
			}
			out = append(out, c...)
		default:
			out = append(out, data...)
		}
	}
	return out, nil
}

// compareDataFiles compares two data files structurally; nil if they hold
// the same pages, else an error describing the first differences.
func compareDataFiles(a, b string) error {
	ia, err := loadImage(a)
	if err != nil {
		return fmt.Errorf("%s: %w", a, err)
	}
	ib, err := loadImage(b)
	if err != nil {
		return fmt.Errorf("%s: %w", b, err)
	}
	if ia.f != ib.f {
		return fmt.Errorf("%s is %s, %s is %s", a, ia.f.name, b, ib.f.name)
	}
	var diffs []string
	if ia.psize != ib.psize {
		diffs = append(diffs, fmt.Sprintf("page sizes %d and %d", ia.psize, ib.psize))
	} else {
		ma, _ := ia.metas()
		mb, _ := ib.metas()
		if !bytes.Equal(ma, mb) {
			diffs = append(diffs, "meta pages differ")
		}
	}
	var onlyA, onlyB []uint64
	for p := range ia.pages {
		if _, ok := ib.pages[p]; !ok {
			onlyA = append(onlyA, p)
		}
	}
	for p := range ib.pages {
		if _, ok := ia.pages[p]; !ok {
			onlyB = append(onlyB, p)
		}
	}
	if len(onlyA)+len(onlyB) > 0 {
		sort.Slice(onlyA, func(i, j int) bool { return onlyA[i] < onlyA[j] })
		sort.Slice(onlyB, func(i, j int) bool { return onlyB[i] < onlyB[j] })
		diffs = append(diffs, fmt.Sprintf("reachable pages differ: only in the first %v, only in the second %v", onlyA, onlyB))
	}
	keys := make([]uint64, 0, len(ia.pages))
	for p := range ia.pages {
		keys = append(keys, p)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, p := range keys {
		ca := ia.pages[p]
		cb, ok := ib.pages[p]
		if ok && !bytes.Equal(ca, cb) {
			at := 0
			for at < len(ca) && at < len(cb) && ca[at] == cb[at] {
				at++
			}
			diffs = append(diffs, fmt.Sprintf("page %d differs at canonical byte %d", p, at))
		}
	}
	if len(diffs) == 0 {
		return nil
	}
	n := len(diffs)
	if len(diffs) > 12 {
		diffs = diffs[:12]
	}
	return fmt.Errorf("%s and %s differ in %d ways:\n  %s", a, b, n, strings.Join(diffs, "\n  "))
}
