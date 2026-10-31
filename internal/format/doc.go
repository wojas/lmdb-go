// Package format parses and builds LMDB's on-disk structures: pages,
// nodes, meta pages and DB records of the LMDB 0.9 and 1.0 data files,
// the comparators that define the sort order on disk, ID lists, and the
// numeric constants (flags, error codes, cursor operations) of lmdb.h.
//
// It is the Go form of rlmdb's format module (format/*.rs minus the lock
// file), consts.rs, flags.rs, error.rs, cmp.rs and idl.rs. Everything works
// on byte slices; integers are read in host byte order, as LMDB stores
// them, and every access is bounds-checked: malformed input yields "not
// ok" results that callers turn into MDB_CORRUPTED, never a panic.
package format
