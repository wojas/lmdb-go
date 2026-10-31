// Data-file format identifiers (rlmdb format/layout.rs, Format).

package format

// Format is a data-file format, named after the C release that writes it.
type Format uint8

const (
	// V09 is LMDB 0.9 (MDB_DATA_VERSION 1), what every 0.9.x release
	// reads and writes.
	V09 Format = 9
	// V10 is LMDB 1.0 (MDB_DATA_VERSION 3).
	V10 Format = 10
)

// String returns the name of the format as users write it: "0.9" or
// "1.0" (the values of RLMDB_DEFAULT_FORMAT).
func (f Format) String() string {
	switch f {
	case V09:
		return "0.9"
	case V10:
		return "1.0"
	}
	return "unknown"
}

// FormatFromName returns the format called name ("0.9" or "1.0", exactly).
func FormatFromName(name string) (Format, bool) {
	switch name {
	case "0.9":
		return V09, true
	case "1.0":
		return V10, true
	}
	return 0, false
}
