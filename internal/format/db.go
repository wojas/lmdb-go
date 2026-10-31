// MDB_db: the 48-byte record describing one B-tree (rlmdb format/db.rs).
//
// Two live in every meta page (mm_dbs[FREE_DBI], mm_dbs[MAIN_DBI]); named
// databases store one as the data of an F_SUBDATA node in the main DB, and
// MDB_DUPSORT keys whose duplicates outgrew a sub-page store one as the
// data of an F_DUPDATA|F_SUBDATA node.

package format

// DB is a decoded MDB_db record (mdb.c line 1058).
type DB struct {
	// Pad is md_pad: the page size in mm_dbs[FREE_DBI]; the fixed item
	// size (P_LEAF2 key size) of an MDB_DUPFIXED sub-DB.
	Pad uint32
	// Flags is md_flags: persistent DB flags; in mm_dbs[FREE_DBI] the
	// persistent environment flags.
	Flags uint16
	// Depth is md_depth (0 when empty).
	Depth         uint16
	BranchPages   uint64
	LeafPages     uint64
	OverflowPages uint64
	Entries       uint64
	// Root is md_root, PInvalid for an empty tree.
	Root uint64
}

// EmptyDB is an empty tree with the given persistent flags.
func EmptyDB(flags uint16) DB { return DB{Flags: flags, Root: PInvalid} }

// ReadDB decodes a record from the first DBSize bytes of b.
func ReadDB(b []byte) (DB, bool) {
	if len(b) < DBSize {
		return DB{}, false
	}
	return DB{
		Pad:           ne.Uint32(b[dbPad:]),
		Flags:         ne.Uint16(b[dbFlags:]),
		Depth:         ne.Uint16(b[dbDepth:]),
		BranchPages:   ne.Uint64(b[dbBranchPages:]),
		LeafPages:     ne.Uint64(b[dbLeafPages:]),
		OverflowPages: ne.Uint64(b[dbOverflowPages:]),
		Entries:       ne.Uint64(b[dbEntries:]),
		Root:          ne.Uint64(b[dbRoot:]),
	}, true
}

// Write encodes the record into the first DBSize bytes of b (which must
// hold them).
func (d *DB) Write(b []byte) {
	_ = b[DBSize-1]
	ne.PutUint32(b[dbPad:], d.Pad)
	ne.PutUint16(b[dbFlags:], d.Flags)
	ne.PutUint16(b[dbDepth:], d.Depth)
	ne.PutUint64(b[dbBranchPages:], d.BranchPages)
	ne.PutUint64(b[dbLeafPages:], d.LeafPages)
	ne.PutUint64(b[dbOverflowPages:], d.OverflowPages)
	ne.PutUint64(b[dbEntries:], d.Entries)
	ne.PutUint64(b[dbRoot:], d.Root)
}

// Bytes returns the record's 48 on-disk bytes.
func (d *DB) Bytes() [DBSize]byte {
	var b [DBSize]byte
	d.Write(b[:])
	return b
}
