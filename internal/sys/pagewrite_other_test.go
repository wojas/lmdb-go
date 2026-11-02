//go:build !linux

package sys

// iovRefs: outside Linux, PageWriter keeps no iovecs.
func (w *PageWriter) iovRefs() []bool { return nil }
