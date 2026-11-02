// SniffFormat, lmdb-go v2's format detection (lmdb-go v2 lmdb/sniff.go),
// on the engine's reading of the first meta page (internal/format,
// rlmdb's format::sniff_format).

package lmdb

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/PowerDNS/lmdb-go/internal/engine"
	"github.com/PowerDNS/lmdb-go/internal/format"
)

// sniffMagic is mm_magic, 0xBEEFC0DE in both formats.
const sniffMagic = format.Magic

// ErrFormatUnsupported is returned when a database file carries a valid LMDB
// magic but data format version 2, which belongs to neither LMDB 0.9 nor 1.0.
// It is written by lmdb-js, whose vendored LMDB prerelease snapshot defines
// MDB_DATA_VERSION 2.
var ErrFormatUnsupported = errors.New("lmdb: unsupported database format (written by an lmdb-js prerelease)")

// SniffFormat detects the on-disk format of the database at path, without
// opening the environment: from the magic and version of the first meta
// page, the only part of the file whose position both formats agree on
// (after a page header of 16 bytes in 0.9 and 24 in 1.0).
//
// Of flags, only NoSubdir is honored (path is the data file itself rather
// than a directory containing data.mdb).
//
// A missing or empty data file yields (FormatUnknown, nil): the environment
// does not exist yet and Open would create it. A file that is not an LMDB
// database yields an OpError wrapping Invalid; a valid LMDB file of an
// unknown data version yields an OpError wrapping VersionMismatch; the
// lmdb-js prerelease format yields ErrFormatUnsupported. Other errors are those of
// opening and reading the file.
func SniffFormat(path string, flags uint) (Format, error) {
	datafile := path
	if flags&NoSubdir == 0 {
		datafile = filepath.Join(path, "data.mdb")
	}

	f, err := os.Open(datafile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return FormatUnknown, nil
		}
		return FormatUnknown, err
	}
	// The name may lead to the lock file of an environment open in this
	// process, whose record locks closing it would drop.
	defer engine.Dispose(f)

	var buf [format.SniffLen]byte
	n, err := io.ReadFull(f, buf[:])
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return FormatUnknown, err
	}
	v, ok, err := format.SniffFormat(buf[:n])
	switch {
	case err == format.VersionMismatch:
		return FormatUnknown, ErrFormatUnsupported
	case err != nil && hasMagic(buf[:n]):
		// Valid magic, unknown data version (e.g. MDB_DEVEL builds): what
		// mdb_env_read_header says, as lmdb-go v2 does (rlmdb says
		// MDB_INVALID).
		return FormatUnknown, &OpError{Op: "sniff", Errno: VersionMismatch}
	case err != nil:
		return FormatUnknown, &OpError{Op: "sniff", Errno: Invalid}
	case !ok:
		return FormatUnknown, nil
	}
	return Format(v), nil
}

// hasMagic reports whether head has the meta page's magic where either
// format has it.
func hasMagic(head []byte) bool {
	for _, l := range []*format.Layout{format.LayoutV09, format.LayoutV10} {
		if m, ok := format.Get32(head, l.MetaOffset()); ok && m == sniffMagic {
			return true
		}
	}
	return false
}
