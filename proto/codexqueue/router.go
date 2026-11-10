package codexqueue

// Router is the receiver chain's tail for the queue route: the mailbox write commits
// first, and only then does the adapter's MailSignal (Wakes.Signal) run.
type Router struct {
	MB    *Mailbox
	W     *Wakes
	Crash func(point string) bool
}

func (r *Router) Receive(thread, id string, wake bool) error {
	r.MB.CommitWake(thread, id, wake)
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
	if !res.More && r.MB.HasWakeEligible(thread) {
		// a wake-eligible entry arrived after this pull's snapshot, while the old
		// generation was still open: the model got More=false, so it needs a new wake.
		r.W.Signal(thread)
	}
	return res
}
