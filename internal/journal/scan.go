package journal

import "os"

// AcceptRecord is Accept for a full dedupe record, so the acceptance and the
// message it carries are one fsynced line.
func (j *Journal) AcceptRecord(r Record) (prior string, dup bool, err error) {
	r.Type = TypeDedupe
	j.mu.Lock()
	defer j.mu.Unlock()
	if l, ok := j.latest[r.key()]; ok {
		return l.State, true, nil
	}
	_, err = j.appendLocked(r)
	return r.State, false, err
}

// Records returns every record on disk in append order.
func (j *Journal) Records() ([]Record, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return nil, ErrClosed
	}
	rs, _, err := readAll(j.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return rs, err
}
