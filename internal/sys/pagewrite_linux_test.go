package sys

// iovRefs reports which iovec slots of w still point to a buffer.
func (w *PageWriter) iovRefs() []bool {
	r := make([]bool, len(w.iov))
	for i := range w.iov {
		r[i] = w.iov[i].Base != nil
	}
	return r
}
