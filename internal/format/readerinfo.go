// Reader-table entries as the engine and the lock file exchange them
// (rlmdb core/reader.rs, ReaderInfo and format_reader_list).

package format

import (
	"fmt"
	"strings"
)

// ReaderInfo is one occupied slot of the reader table (mdb_reader_list).
type ReaderInfo struct {
	PID    int32  // mr_pid: the process owning the slot
	Thread uint64 // mr_tid: pthread_t of the thread that claimed it
	// TxnID is the snapshot in use (mr_txnid); InTxn is false if the slot
	// is owned but idle (mr_txnid == (txnid_t)-1).
	TxnID uint64
	InTxn bool
}

// FormatReaderList formats entries exactly as mdb_reader_list prints them.
func FormatReaderList(readers []ReaderInfo) string {
	if len(readers) == 0 {
		return "(no active readers)\n"
	}
	var b strings.Builder
	b.WriteString("    pid     thread     txnid\n")
	for _, r := range readers {
		if r.InTxn {
			fmt.Fprintf(&b, "%10d %x %d\n", r.PID, r.Thread, r.TxnID)
		} else {
			fmt.Fprintf(&b, "%10d %x -\n", r.PID, r.Thread)
		}
	}
	return b.String()
}
