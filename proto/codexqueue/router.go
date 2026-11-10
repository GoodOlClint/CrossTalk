package codexqueue

// Router is the receiver chain's tail for the queue route: the mailbox write commits
// first, and only then does the adapter's MailSignal (Wakes.Signal) run.
type Router struct {
	MB    *Mailbox
	W     *Wakes
	Crash func(point string) bool
}

func (r *Router) Receive(thread, id string, wake bool) error {
	r.MB.Commit(thread, id)
	if r.Crash != nil && r.Crash("before-signal") {
		return ErrCrash
	}
	if wake {
		_, err := r.W.Signal(thread)
		return err
	}
	return nil
}

func (r *Router) Pull(thread, after string, limit int) PullResult {
	res := r.MB.Pull(thread, after, limit)
	r.W.Pulled(thread)
	return res
}
