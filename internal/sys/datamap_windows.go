// The data file map on Windows: mdb_env_map of the C release of each data
// format (0.9.37 mdb.c line 4110; LMDB 1.0, mdb10.c line 5076) and rlmdb's
// os/windows.rs (map_data, DataMap::map_readonly, DataMap::map_reserved,
// DataMap::commit_pages), whose module documentation records the measured
// behaviour these decisions rest on. Both views are read-only
// (PAGE_READONLY).
//
//   - LMDB 0.9: CreateFileMapping and MapViewOfFile(FILE_MAP_READ) of the
//     whole map size; the mapping handle is closed at once (the view keeps
//     the section alive). Windows maps at most the length of the file, so C
//     0.9 sets the data file of a writable environment to the map size
//     before mapping it: on Windows a 0.9 data file is as long as its map,
//     not as its used pages, and a C 0.9 process with a read-only
//     environment, which maps the file as it is and reads pages past its
//     view unchecked, relies on that. The engine extends a shorter file the
//     same way before MapData maps it (ExtendFile), but never shortens a
//     longer one, and it reads the size and extends the file holding the
//     writer mutex: C's SetEndOfFile also shortens a longer file, by a size
//     decided from a meta page read before, and so cuts pages another
//     process committed since (rlmdb KNOWN_C_BUGS.md row 57). C 0.9 also
//     shrinks the file when the map size shrinks, which fails
//     (ERROR_USER_MAPPED_FILE) while another process, or a read
//     transaction of this one holding an older map, still maps more.
//   - LMDB 1.0: a section created with SEC_RESERVE over the file as it is
//     (NtCreateSection) and a view that reserves the map size
//     (NtMapViewOfSection with MEM_RESERVE). The file grows with the
//     writes. Writing past the end of the file does not make the pages
//     readable in such a view: all sections of a file share one segment,
//     which past the first MiB covers only what was committed or extended.
//     C 1.0's mdb_page_alloc therefore commits every page a write
//     transaction takes from the end of the map in its view
//     (VirtualAlloc(MEM_COMMIT), mdb10.c line 2876), which extends the file
//     and the shared segment, so that every process's view shows the pages
//     before the commit that writes them is published. CommitPages does the
//     same (the engine calls it where it takes pages from the end of the
//     map): a writer that did not would leave C 1.0 processes faulting on
//     its pages. As a second line of defence a writable map keeps its
//     section and extends it to the file's size (NtExtendSection, never past
//     it) before it hands out a page beyond the size it last knew. C 1.0
//     closes the section after mapping.
//   - A read-only handle maps the file as it is (C maps with size 0,
//     "whatever exists", for MDB_RDONLY; Windows refuses a reserved view of
//     a read-only section unless it allows the reservation, which is tried
//     first for 1.0). Pages added later are refused by Slice (an error, not
//     a fault), and when a snapshot needs them the engine maps the file
//     again (Env.renewShortMap, which never applies on Unix: there the map
//     is always the map size).
//   - MDB_NORDAHEAD has no effect (C ignores it on Windows too).
//
// The view becomes a []byte with unsafe.Slice; that is the package's only
// unsafe code besides passing pointers to the system calls.

package sys

import (
	"os"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// osMap: a duplicate of the data file's handle, to refresh the file size,
// and the section of a reserved view (LMDB 1.0 through a writable handle),
// kept to extend it as the file grows (0 for the other maps). Both are
// closed with the mapping.
type osMap struct {
	file    windows.Handle
	section windows.Handle
}

// The NT calls C 1.0 makes for its reserved map, which the Win32 API does
// not offer (golang.org/x/sys/windows does not declare them either).
var (
	ntdll                  = windows.NewLazySystemDLL("ntdll.dll")
	procNtCreateSection    = ntdll.NewProc("NtCreateSection")
	procNtMapViewOfSection = ntdll.NewProc("NtMapViewOfSection")
	procNtExtendSection    = ntdll.NewProc("NtExtendSection")
	procGetSystemInfo      = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetSystemInfo")
)

const (
	secReserve        = 0x04000000 // SEC_RESERVE
	sectionMapRead    = 0x0004     // SECTION_MAP_READ
	sectionExtendSize = 0x0010     // SECTION_EXTEND_SIZE
	viewUnmap         = 2          // ViewUnmap (SECTION_INHERIT)
)

// MapData maps size bytes of the data file f read-only, as the C release
// of its format does on Windows (see the file comment): with reserve
// (LMDB 1.0) a reserved view of a SEC_RESERVE section that the file grows
// into, otherwise (LMDB 0.9) a view of the map size, or of the whole file
// if it is shorter: the data file of a writable environment is extended
// first (ExtendFile), a read-only one is mapped as it is. random (MDB_NORDAHEAD) has no effect. metaEnd
// is the end of the meta pages, which Slice refuses. The map keeps a
// duplicate of f's handle to refresh the file size, closed with the
// mapping. It starts with one reference, the caller's.
func MapData(f *os.File, size int, random bool, metaEnd int, reserve bool) (*DataMap, error) {
	if size <= 0 {
		return nil, syscall.EINVAL
	}
	h, err := dupHandle(windows.Handle(f.Fd()))
	if err != nil {
		return nil, err
	}
	m := &DataMap{metaEnd: metaEnd, osMap: osMap{file: h}}
	var fileSize int64
	if reserve {
		m.data, m.section, fileSize, err = mapReserved(h, size)
	} else {
		m.data, fileSize, err = mapWhole(h, size)
	}
	if err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	m.knownSize.Store(fileSize)
	m.refs.Store(1)
	return m, nil
}

// mapWhole is mdb_env_map of LMDB 0.9: as much of size as the file holds
// is mapped (extending the file is the caller's, ExtendFile). Returns the
// view and the file's size.
func mapWhole(h windows.Handle, size int) ([]byte, int64, error) {
	fileSize, err := handleSize(h)
	if err != nil {
		return nil, 0, err
	}
	n := min(fileSize, int64(size))
	if n == 0 {
		// Windows cannot map an empty file (C fails the same way).
		return nil, 0, windows.ERROR_FILE_INVALID
	}
	data, err := MapView(h, int(n), false)
	return data, fileSize, err
}

// mapReserved is mdb_env_map of LMDB 1.0: a SEC_RESERVE section over the
// file as it is and a view of it that reserves size bytes of address
// space. Returns the view, the section if the map keeps it (writable
// handle; else 0, the view keeps the section alive as in C) and the file's
// size when it was mapped.
func mapReserved(h windows.Handle, size int) ([]byte, windows.Handle, int64, error) {
	fileSize, err := handleSize(h)
	if err != nil {
		return nil, 0, 0, err
	}
	if err := procNtMapViewOfSection.Find(); err != nil {
		return nil, 0, 0, err
	}
	// C asks for a writable section for a writable environment (the view
	// itself is read-only), a read-only one for MDB_RDONLY.
	writable := true
	section, st := createSection(h, windows.PAGE_READWRITE, sectionMapRead|sectionExtendSize)
	if st == windows.STATUS_ACCESS_DENIED {
		writable = false
		section, st = createSection(h, windows.PAGE_READONLY, sectionMapRead)
	}
	if failed(st) {
		return nil, 0, 0, st.Errno()
	}
	// A writable environment reserves the map size, as C 1.0 does. A
	// read-only one does too where Windows allows it, so that it sees the
	// pages other processes add (C 1.0 maps what exists then, and so does
	// this if the reservation is refused).
	data, st := mapSection(section, size, true)
	if failed(st) && !writable {
		data, st = mapSection(section, size, false)
	}
	keep := writable && !failed(st)
	if !keep {
		windows.CloseHandle(section)
		section = 0
	}
	if failed(st) {
		return nil, 0, 0, st.Errno()
	}
	return data, section, fileSize, nil
}

// failed reports whether an NTSTATUS is an error (its sign bit set).
func failed(st windows.NTStatus) bool { return int32(st) < 0 }

// createSection is NtCreateSection(SEC_RESERVE) over the file h with no
// maximum size (the file's size).
func createSection(h windows.Handle, protect, access uint32) (windows.Handle, windows.NTStatus) {
	if err := procNtCreateSection.Find(); err != nil {
		return 0, windows.STATUS_PROCEDURE_NOT_FOUND
	}
	var section windows.Handle
	r, _, _ := procNtCreateSection.Call(
		uintptr(unsafe.Pointer(&section)),
		uintptr(access),
		0, // no object attributes
		0, // no maximum size: the file's
		uintptr(protect),
		secReserve,
		uintptr(h),
	)
	return section, windows.NTStatus(uint32(r))
}

// mapSection is NtMapViewOfSection of the section, read-only: with
// reserve, a view of size bytes that reserves what the section does not
// cover yet (MEM_RESERVE); otherwise a view of the whole section.
func mapSection(section windows.Handle, size int, reserve bool) ([]byte, windows.NTStatus) {
	var base unsafe.Pointer
	var viewSize uintptr
	var allocType uintptr
	if reserve {
		viewSize, allocType = uintptr(size), windows.MEM_RESERVE
	}
	r, _, _ := procNtMapViewOfSection.Call(
		uintptr(section),
		uintptr(windows.CurrentProcess()),
		uintptr(unsafe.Pointer(&base)),
		0, // zero bits
		0, // commit size
		0, // section offset: 0
		uintptr(unsafe.Pointer(&viewSize)),
		viewUnmap,
		allocType,
		windows.PAGE_READONLY,
	)
	st := windows.NTStatus(uint32(r))
	if failed(st) {
		return nil, st
	}
	n := int(viewSize)
	if reserve {
		n = min(size, n)
	}
	// unsafe: base is the start of a view of at least n bytes, which stays
	// mapped until unmap; nothing writes it through this mapping.
	return unsafe.Slice((*byte)(base), n), st
}

// viewSize is the size of the data file, which a reserved view shows once
// its section is extended to it (never past the view).
func (m *DataMap) viewSize() (int64, error) {
	size, err := handleSize(m.file)
	if err != nil {
		return 0, err
	}
	if m.section != 0 {
		size = m.extendSection(size)
	}
	return size, nil
}

// extendSection extends the section of a reserved view to fileSize (at
// most to the view's length) and returns the size the view now shows, or
// the size known before if that fails. It never makes the section larger
// than the file, so the file is not changed.
func (m *DataMap) extendSection(fileSize int64) int64 {
	want := min(fileSize, int64(len(m.data)))
	size := want
	if procNtExtendSection.Find() != nil {
		return m.knownSize.Load()
	}
	r, _, _ := procNtExtendSection.Call(uintptr(m.section), uintptr(unsafe.Pointer(&size)))
	if failed(windows.NTStatus(uint32(r))) {
		return m.knownSize.Load()
	}
	return min(want, size)
}

// unmap removes the view and closes the section and the file handle.
func (m *DataMap) unmap() {
	_ = UnmapView(m.data)
	if m.section != 0 {
		windows.CloseHandle(m.section)
	}
	windows.CloseHandle(m.file)
}

// Invalidate does nothing on Windows: MapNeedsInvalidate is false, the map
// and the file share one cache there (MDB_Invalidate is OpenBSD's).
func (m *DataMap) Invalidate(off uint64, n int) {}

// CommitPages commits n bytes at off of a reserved view, the pages a write
// transaction takes from the end of the map (C 1.0's mdb_page_alloc,
// mdb10.c line 2876: VirtualAlloc(MEM_COMMIT, PAGE_READONLY)). That extends
// the file by these pages and the segment the file's sections share, so
// they show in every process's view once written (see the file comment).
// A no-op for the other maps (LMDB 0.9: the file already has the map
// size).
func (m *DataMap) CommitPages(off, n int) error {
	if m.section == 0 {
		return nil
	}
	end := off + n
	if off < 0 || n <= 0 || end < off || end > len(m.data) {
		return syscall.EINVAL
	}
	// Committing pages of a reserved view of a file-backed section only
	// makes them readable: it changes no bytes a slice handed out could see.
	_, err := windows.VirtualAlloc(uintptr(unsafe.Pointer(&m.data[off])), uintptr(n), windows.MEM_COMMIT, windows.PAGE_READONLY)
	return err
}

// MapView maps the first n bytes of the file h, which must be at least that
// long, read-only (PAGE_READONLY, FILE_MAP_READ: the data file) or
// read/write (PAGE_READWRITE, FILE_MAP_WRITE: the lock file, mdb.c line
// 4937). The mapping handle is closed at once; the view keeps the section
// alive, as in C. UnmapView removes the view.
func MapView(h windows.Handle, n int, writable bool) ([]byte, error) {
	if n <= 0 {
		return nil, syscall.EINVAL
	}
	protect, access := uint32(windows.PAGE_READONLY), uint32(windows.FILE_MAP_READ)
	if writable {
		protect, access = windows.PAGE_READWRITE, windows.FILE_MAP_WRITE
	}
	size := uint64(n)
	mh, err := windows.CreateFileMapping(h, nil, protect, uint32(size>>32), uint32(size), nil)
	if err != nil {
		return nil, err
	}
	addr, err := windows.MapViewOfFile(mh, access, 0, 0, uintptr(n))
	windows.CloseHandle(mh)
	if err != nil {
		return nil, err
	}
	// unsafe: addr is the start of a view of n bytes, outside the Go heap,
	// mapped until UnmapView. Read through a *uintptr so that no uintptr
	// value is converted to a pointer (go vet's unsafeptr rule).
	base := *(*unsafe.Pointer)(unsafe.Pointer(&addr))
	return unsafe.Slice((*byte)(base), n), nil
}

// UnmapView removes a view made by MapView or MapData (UnmapViewOfFile).
// Nothing may use b afterwards.
func UnmapView(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	return windows.UnmapViewOfFile(uintptr(unsafe.Pointer(unsafe.SliceData(b))))
}

// OSPageSize is the OS memory page size (GetSystemInfo's dwPageSize, 4096
// on x86-64 and arm64), as C's GET_PAGESIZE.
func OSPageSize() int { return osPageSize() }

var osPageSize = sync.OnceValue(func() int {
	// SYSTEM_INFO (64-bit layout); only dwPageSize is used.
	var si struct {
		oemID                       uint32
		pageSize                    uint32
		minAddr, maxAddr, procMask  uintptr
		numProcs, procType, granule uint32
		level, revision             uint16
	}
	if procGetSystemInfo.Find() == nil {
		procGetSystemInfo.Call(uintptr(unsafe.Pointer(&si)))
	}
	if si.pageSize > 0 {
		return int(si.pageSize)
	}
	return 4096
})

// dupHandle duplicates h within this process, with the same access.
func dupHandle(h windows.Handle) (windows.Handle, error) {
	p := windows.CurrentProcess()
	var dup windows.Handle
	if err := windows.DuplicateHandle(p, h, p, &dup, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return 0, err
	}
	return dup, nil
}

// handleSize is the size of the open file h.
func handleSize(h windows.Handle) (int64, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return 0, err
	}
	return int64(info.FileSizeHigh)<<32 | int64(info.FileSizeLow), nil
}
