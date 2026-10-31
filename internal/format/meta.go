// MDB_meta: the root of a snapshot, stored right after the page header of
// pages 0 and 1 (rlmdb format/meta.rs). The structure is the same in both
// formats; mm_version tells them apart.
//
// Transaction N writes meta page N % 2 (mdb_env_write_meta), so the two
// pages hold the two most recent snapshots and the valid one with the
// larger mm_txnid is current (mdb_env_pick_meta). Meta pages are rewritten
// while readers look at them, so callers decode a private copy.

package format

// Meta is a decoded MDB_meta (mdb.c line 1096).
type Meta struct {
	Magic   uint32 // mm_magic: must be Magic
	Version uint32 // mm_version: the layout's DataVersion
	Address uint64 // mm_address: fixed map address (MDB_FIXEDMAP), normally 0
	MapSize uint64 // mm_mapsize
	// DBs is mm_dbs: [FREE_DBI, MAIN_DBI].
	DBs [2]DB
	// LastPgno is mm_last_pg: the last page in use.
	LastPgno uint64
	// TxnID is mm_txnid: the transaction that committed this snapshot.
	TxnID uint64
}

// ReadMeta decodes a meta from the first MetaSize bytes of b (which starts
// at the MDB_meta, MetaOffset bytes into the page).
func ReadMeta(b []byte) (Meta, bool) {
	if len(b) < MetaSize {
		return Meta{}, false
	}
	m := Meta{
		Magic:    ne.Uint32(b[metaMagic:]),
		Version:  ne.Uint32(b[metaVersion:]),
		Address:  ne.Uint64(b[metaAddress:]),
		MapSize:  ne.Uint64(b[MetaMapSize:]),
		LastPgno: ne.Uint64(b[MetaLastPgno:]),
		TxnID:    ne.Uint64(b[MetaTxnID:]),
	}
	m.DBs[0], _ = ReadDB(b[metaFreeDB:])
	m.DBs[1], _ = ReadDB(b[metaMainDB:])
	return m, true
}

// Write encodes the meta into the first MetaSize bytes of b.
func (m *Meta) Write(b []byte) {
	_ = b[MetaSize-1]
	ne.PutUint32(b[metaMagic:], m.Magic)
	ne.PutUint32(b[metaVersion:], m.Version)
	ne.PutUint64(b[metaAddress:], m.Address)
	ne.PutUint64(b[MetaMapSize:], m.MapSize)
	m.DBs[0].Write(b[metaFreeDB:])
	m.DBs[1].Write(b[metaMainDB:])
	ne.PutUint64(b[MetaLastPgno:], m.LastPgno)
	ne.PutUint64(b[MetaTxnID:], m.TxnID)
}

// ReadMetaPage decodes and validates the meta page in page (header
// included) as mdb_env_read_header does: a P_META page (else Invalid),
// the magic (Invalid), the layout's version (VersionMismatch).
func ReadMetaPage(page []byte, l *Layout) (Meta, error) {
	flags, ok := Get16(page, l.FlagsOffset())
	if !ok || flags&PMeta != PMeta {
		return Meta{}, Invalid
	}
	if len(page) < l.MetaOffset() {
		return Meta{}, Invalid
	}
	m, ok := ReadMeta(page[l.MetaOffset():])
	if !ok {
		return Meta{}, Invalid
	}
	if err := m.Validate(l); err != nil {
		return Meta{}, err
	}
	return m, nil
}

// Validate checks the magic and that the version is the layout's.
func (m *Meta) Validate(l *Layout) error {
	if m.Magic != Magic {
		return Invalid
	}
	if m.Version != l.DataVersion {
		return VersionMismatch
	}
	return nil
}

// PageSize is mm_psize (alias of mm_dbs[FREE_DBI].md_pad).
func (m *Meta) PageSize() uint32 { return m.DBs[0].Pad }

// EnvFlags is mm_flags (alias of mm_dbs[FREE_DBI].md_flags).
func (m *Meta) EnvFlags() uint16 { return m.DBs[0].Flags }

// NewEnvMeta is the meta of a brand-new environment (mdb_env_init_meta0):
// C stores (env_flags & 0xffff) | MDB_INTEGERKEY in the free DB's flags.
func NewEnvMeta(l *Layout, pageSize uint32, mapSize uint64, envFlags uint32) Meta {
	m := Meta{Magic: Magic, Version: l.DataVersion, MapSize: mapSize, LastPgno: 1}
	m.DBs[0] = EmptyDB(uint16(envFlags&0xffff) | uint16(IntegerKey))
	m.DBs[0].Pad = pageSize
	m.DBs[1] = EmptyDB(0)
	return m
}

// WriteInitialPages writes the two initial meta pages into buf (2 *
// pageSize bytes), as mdb_env_init_meta does: pages 0 and 1, both P_META,
// both holding m; everything else zero.
func (m *Meta) WriteInitialPages(buf []byte, pageSize int, l *Layout) {
	clear(buf)
	for i := 0; i < 2; i++ {
		page := buf[i*pageSize : (i+1)*pageSize]
		Put64(page, 0, uint64(i))
		Put16(page, l.FlagsOffset(), PMeta)
		m.Write(page[l.MetaOffset():])
	}
}

// SniffLen is how many bytes at the start of a data file SniffFormat
// looks at.
const SniffLen = 32

// prereleaseDataVersion is mm_version of the prerelease 1.0 format some
// lmdb-js builds wrote.
const prereleaseDataVersion = 2

// SniffFormat tells which format a data file is in from its first SniffLen
// bytes, trusting only the magic and version of the first meta page:
// empty head: (0, false, nil), a new environment; magic at 16 and version
// 1: V09; magic at 24 and version 3: V10; version 2 at either place:
// VersionMismatch; anything else: Invalid.
func SniffFormat(head []byte) (Format, bool, error) {
	if len(head) == 0 {
		return 0, false, nil
	}
	for _, l := range []*Layout{LayoutV09, LayoutV10} {
		at := l.MetaOffset()
		if v, ok := Get32(head, at+metaMagic); !ok || v != Magic {
			continue
		}
		v, ok := Get32(head, at+metaVersion)
		switch {
		case ok && v == l.DataVersion:
			return l.Format, true, nil
		case ok && v == prereleaseDataVersion:
			return 0, false, VersionMismatch
		}
	}
	return 0, false, Invalid
}

// PickNewest is mdb_env_pick_meta: metas[metas[0].txnid < metas[1].txnid].
func PickNewest(metas *[2]Meta) int {
	if metas[0].TxnID < metas[1].TxnID {
		return 1
	}
	return 0
}
