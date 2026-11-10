package journal

import (
	"errors"
	"os"
)

// ErrIDInUse: another sender already holds the message_id.
var ErrIDInUse = errors.New("journal: message_id is held by another sender")

// AcceptRecord is Accept for a full dedupe record, so the acceptance and the
// message it carries are one fsynced line.
func (j *Journal) AcceptRecord(r Record) (prior string, dup bool, err error) {
	r.Type = TypeDedupe
	j.mu.Lock()
	defer j.mu.Unlock()
	if l, ok := j.latest[r.key()]; ok {
		return l.State, true, nil
	}
	for s := range j.idSenders[r.MessageID] {
		if s != r.Sender {
			return "", false, ErrIDInUse
		}
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

// IDHeldByOther reports whether a sender other than sender has a dedupe record
// for messageID.
func (j *Journal) IDHeldByOther(sender, messageID string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	for s := range j.idSenders[messageID] {
		if s != sender {
			return true
		}
	}
	return false
}

// DedupeSender returns the sender key holding the dedupe record for
// messageID; a message_id has at most one.
func (j *Journal) DedupeSender(messageID string) (string, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for s := range j.idSenders[messageID] {
		return s, true
	}
	return "", false
}
