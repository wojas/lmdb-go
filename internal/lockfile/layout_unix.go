//go:build unix

// mr_tid on Unix: a pthread_t (8 bytes on the 64-bit platforms LMDB's
// format supports here) after the pid and its padding.

package lockfile

const (
	// ReaderTID is the offset of mr_tid (pthread_t) in a reader slot.
	ReaderTID = 16
	// ReaderTIDSize is the size of mr_tid: 8 (pthread_t).
	ReaderTIDSize = 8
)
