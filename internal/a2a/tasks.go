package a2a

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/journal"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
)

const (
	defaultPageSize = 50
	maxPageSize     = 100
)

type task struct {
	id        sdk.TaskID
	contextID string
	principal string
	messageID string
	handles   []string
	kind      msg.Kind
	state     sdk.TaskState
	delivery  string
	reason    string
	history   []*sdk.Message
	status    *sdk.Message
	updated   time.Time
	deadline  time.Time
	done      chan struct{}
	timer     *time.Timer
}

type store struct {
	mu        sync.Mutex
	byID      map[sdk.TaskID]*task
	byMsg     map[string]*task
	clientIDs map[[2]string]string
	stopped   bool
	ts        TaskStore
	log       *slog.Logger
}

func newStore(ts TaskStore, log *slog.Logger) *store {
	st := &store{byID: map[sdk.TaskID]*task{}, byMsg: map[string]*task{}, clientIDs: map[[2]string]string{}, ts: ts, log: log}
	st.replay(ts.Load())
	return st
}

func (st *store) saveLocked(t *task, appended *sdk.Message) error {
	err := st.ts.Save(t.record(taskStateNames[t.state], appended))
	if err != nil {
		st.log.Warn("a2a: task not journaled", "task", t.id, "message_id", t.messageID, "err", err)
	}
	return err
}

// alias keeps one assigned message_id per (principal, non-UUIDv7 client id),
// so a client retry is deduped like a UUIDv7 retry.
func (st *store) alias(principal, clientID, fresh string) string {
	st.mu.Lock()
	defer st.mu.Unlock()
	k := [2]string{principal, clientID}
	if id, ok := st.clientIDs[k]; ok {
		return id
	}
	st.clientIDs[k] = fresh
	return fresh
}

func (st *store) create(principal string, m msg.Message, wire *sdk.Message, maxAsks, maxPending int) (*task, bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if t, ok := st.byMsg[m.MessageID]; ok {
		if t.principal != principal {
			return nil, false, badMessage("message_id is in use by another principal")
		}
		return t, true, nil
	}
	unresolved := 0
	for _, t := range st.byID {
		if !t.state.Terminal() {
			unresolved++
		}
	}
	if unresolved >= maxPending {
		return nil, false, rejection(m.MessageID, receiver.ReasonTooManyPending)
	}
	if m.Kind == msg.Ask {
		open := 0
		for _, t := range st.byID {
			if t.kind == msg.Ask && !t.state.Terminal() {
				open++
			}
		}
		if open >= maxAsks {
			return nil, false, rejection(m.MessageID, ReasonTooManyAsks)
		}
	}
	t := &task{
		id:        sdk.NewTaskID(),
		contextID: m.ContextID,
		principal: principal,
		messageID: m.MessageID,
		kind:      m.Kind,
		state:     sdk.TaskStateSubmitted,
		delivery:  journal.StateSubmitted,
		updated:   time.Now().UTC(),
		done:      make(chan struct{}),
	}
	if m.Kind == msg.Ask && m.DeadlineSeconds > 0 {
		t.deadline = t.updated.Add(time.Duration(m.DeadlineSeconds) * time.Second)
	}
	wire.TaskID = t.id
	t.history = []*sdk.Message{wire}
	if err := st.ts.Save(t.record(journal.StateSubmitted, wire)); err != nil {
		if errors.Is(err, journal.ErrRecordTooLarge) {
			return nil, false, rejection(m.MessageID, receiver.ReasonTooLarge)
		}
		return nil, false, sdk.NewError(sdk.ErrInternalError, "task could not be recorded")
	}
	st.byID[t.id] = t
	st.byMsg[t.messageID] = t
	return t, false, nil
}

// resumeInbound rebuilds the receiver input of a replayed submitted task from
// its journaled first message. It returns (nil, true) for a task that is not
// submitted, and false when the message or sender cannot be rebuilt.
func (st *store) resumeInbound(t *task, resolve func(string) (receiver.Sender, bool)) (*receiver.Inbound, bool) {
	st.mu.Lock()
	submitted, first := t.state == sdk.TaskStateSubmitted, (*sdk.Message)(nil)
	if len(t.history) > 0 {
		first = t.history[0]
	}
	who := t.principal
	st.mu.Unlock()
	if !submitted {
		return nil, true
	}
	if first == nil {
		return nil, false
	}
	m, err := decode(first)
	if err != nil {
		return nil, false
	}
	var snd receiver.Sender
	switch {
	case who == receiver.PrincipalLocalUser:
		snd = receiver.Sender{Verified: true, Principal: who}
	default:
		ok := false
		if resolve != nil {
			snd, ok = resolve(who)
		}
		if !ok {
			snd = receiver.Sender{Verified: true, AgentID: who}
		}
	}
	return &receiver.Inbound{Ingress: receiver.A2A, Message: m, Sender: snd}, true
}

func (st *store) remove(t *task) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.byID, t.id)
	delete(st.byMsg, t.messageID)
	for _, h := range t.handles {
		delete(st.byMsg, h)
	}
	if err := st.ts.Save(t.record(journal.StateRemoved, nil)); err != nil {
		st.log.Warn("a2a: task removal not journaled", "task", t.id, "err", err)
	}
}

func (st *store) byMessage(messageID string) (*task, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	t, ok := st.byMsg[messageID]
	return t, ok
}

func (st *store) pending() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []string
	for _, t := range st.byID {
		if !t.state.Terminal() {
			out = append(out, t.messageID)
		}
	}
	slices.Sort(out)
	return out
}

// markDispatching records a lost cancel race: the receiver holds the claim.
func (st *store) markDispatching(t *task) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if t.state == sdk.TaskStateSubmitted {
		prev := *t
		t.state, t.delivery = sdk.TaskStateWorking, journal.StateDispatching
		t.updated = time.Now().UTC()
		if st.saveLocked(t, nil) != nil {
			t.state, t.delivery, t.updated = prev.state, prev.delivery, prev.updated
		}
	}
}

// lookup returns the task only to the principal that created it; any other
// principal is told it does not exist (§5).
func (st *store) lookup(id sdk.TaskID, principal string) (*task, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	t, ok := st.byID[id]
	if !ok || t.principal != principal {
		return nil, false
	}
	return t, true
}

func (st *store) setTimer(t *task, d time.Duration, f func()) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.stopped || t.state.Terminal() {
		return
	}
	t.timer = time.AfterFunc(d, f)
}

func (st *store) stopTimers() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.stopped = true
	for _, t := range st.byID {
		if t.timer != nil {
			t.timer.Stop()
		}
	}
}

// update moves a non-terminal task; terminal states are final, so a late
// receiver result never overwrites a cancel, a deadline or a reply. A failed
// journal write leaves the task as it was and returns the error.
func (st *store) update(t *task, state sdk.TaskState, delivery, reason string, status *sdk.Message) (bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.updateLocked(t, state, delivery, reason, status)
}

func (st *store) updateLocked(t *task, state sdk.TaskState, delivery, reason string, status *sdk.Message) (bool, error) {
	if t.state.Terminal() {
		return false, nil
	}
	prev := *t
	t.state = state
	if delivery != "" {
		t.delivery = delivery
	}
	if reason != "" {
		t.reason = reason
	}
	if status != nil {
		t.status = status
		t.history = append(t.history, status)
	}
	t.updated = time.Now().UTC()
	if err := st.saveLocked(t, status); err != nil {
		t.state, t.delivery, t.reason, t.status, t.history, t.updated = prev.state, prev.delivery, prev.reason, prev.status, prev.history, prev.updated
		return false, err
	}
	if state.Terminal() {
		if t.timer != nil {
			t.timer.Stop()
		}
		close(t.done)
	}
	return true, nil
}

// markUnknown holds t at working with an unknown delivery after a harness
// write whose outcome could not be journaled; it is never terminal.
func (st *store) markUnknown(t *task) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if t.state.Terminal() {
		return
	}
	t.state, t.delivery, t.reason = sdk.TaskStateWorking, string(adapter.Unknown), receiver.ReasonJournal
	t.updated = time.Now().UTC()
	_ = st.saveLocked(t, nil)
}

// appendContinuation records a continuation message of t and makes its
// message id a handle that resolves to t.
func (st *store) appendContinuation(t *task, m *sdk.Message) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if o, ok := st.byMsg[m.ID]; ok && o != t {
		return badMessage("message_id is in use by another task")
	}
	prev := *t
	t.history = append(t.history, m)
	t.updated = time.Now().UTC()
	if err := st.saveLocked(t, m); err != nil {
		t.history, t.updated = prev.history, prev.updated
		return err
	}
	st.index(t, m.ID)
	return nil
}

func (st *store) index(t *task, handle string) {
	if handle == "" || handle == t.messageID || slices.Contains(t.handles, handle) {
		return
	}
	t.handles = append(t.handles, handle)
	st.byMsg[handle] = t
}

// askOpen reports whether messageID is a handle of an ask task, that task's
// id, and whether the task is still open.
func (st *store) askOpen(messageID string) (taskID string, open, known bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	t, ok := st.byMsg[messageID]
	if !ok || t.kind != msg.Ask {
		return "", false, false
	}
	return string(t.id), !t.state.Terminal(), true
}

func (st *store) messageIDs(t *task) []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]string{t.messageID}, t.handles...)
}

func (st *store) terminalAsks() []*task {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []*task
	for _, t := range st.byID {
		if t.kind == msg.Ask && t.state.Terminal() {
			out = append(out, t)
		}
	}
	return out
}

func (st *store) completeAsk(messageID, text string) (*task, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	t, ok := st.byMsg[messageID]
	if !ok || t.kind != msg.Ask {
		return nil, ErrNoSuchAsk
	}
	if t.state == sdk.TaskStateCompleted && t.status != nil && messageText(t.status) == text {
		return t, nil
	}
	reply := &sdk.Message{ID: msg.NewUUIDv7(), Role: sdk.MessageRoleAgent, ContextID: t.contextID, TaskID: t.id, Parts: sdk.ContentParts{sdk.NewTextPart(text)}}
	delivery := ""
	if t.delivery == journal.StateSubmitted || t.delivery == journal.StateDispatching {
		delivery = string(adapter.Accepted)
	}
	done, err := st.updateLocked(t, sdk.TaskStateCompleted, delivery, "", reply)
	if err != nil {
		return nil, err
	}
	if !done {
		return nil, ErrAskClosed
	}
	return t, nil
}

func (st *store) snapshot(t *task, historyLength *int) *sdk.Task {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.snapshotLocked(t, historyLength)
}

func (st *store) snapshotLocked(t *task, historyLength *int) *sdk.Task {
	ct := map[string]any{"completion": CompletionDelivered, "message_id": t.messageID, "kind": string(t.kind), "state": t.delivery}
	if t.reason != "" {
		ct["reason"] = t.reason
	}
	ts := t.updated
	h := slices.Clone(t.history)
	if historyLength != nil && *historyLength >= 0 && len(h) > *historyLength {
		h = h[len(h)-*historyLength:]
	}
	return &sdk.Task{
		ID:        t.id,
		ContextID: t.contextID,
		History:   h,
		Metadata:  map[string]any{MetaKey: ct},
		Status:    sdk.TaskStatus{State: t.state, Message: t.status, Timestamp: &ts},
	}
}

func (st *store) list(principal string, req *sdk.ListTasksRequest) (*sdk.ListTasksResponse, error) {
	size := req.PageSize
	switch {
	case size < 0:
		return nil, badMessage("pageSize is negative")
	case size == 0:
		size = defaultPageSize
	case size > maxPageSize:
		size = maxPageSize
	}
	off := 0
	if req.PageToken != "" {
		n, err := strconv.Atoi(req.PageToken)
		if err != nil || n < 0 {
			return nil, badMessage("invalid pageToken")
		}
		off = n
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	var own []*task
	for _, t := range st.byID {
		if t.principal != principal {
			continue
		}
		if req.ContextID != "" && t.contextID != req.ContextID {
			continue
		}
		if req.Status != sdk.TaskStateUnspecified && t.state != req.Status {
			continue
		}
		if req.StatusTimestampAfter != nil && !t.updated.After(*req.StatusTimestampAfter) {
			continue
		}
		own = append(own, t)
	}
	slices.SortFunc(own, func(a, b *task) int {
		if c := b.updated.Compare(a.updated); c != 0 {
			return c
		}
		return compareStr(string(a.id), string(b.id))
	})
	resp := &sdk.ListTasksResponse{TotalSize: len(own), PageSize: size, Tasks: []*sdk.Task{}}
	if off >= len(own) {
		return resp, nil
	}
	end := min(off+size, len(own))
	for _, t := range own[off:end] {
		resp.Tasks = append(resp.Tasks, st.snapshotLocked(t, req.HistoryLength))
	}
	if end < len(own) {
		resp.NextPageToken = strconv.Itoa(end)
	}
	return resp, nil
}

func compareStr(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func failedStatus(t *task, state adapter.State, reason string) *sdk.Message {
	text := string(state)
	if reason != "" {
		text += ": " + reason
	}
	return &sdk.Message{ID: msg.NewUUIDv7(), Role: sdk.MessageRoleAgent, ContextID: t.contextID, TaskID: t.id, Parts: sdk.ContentParts{sdk.NewTextPart(text)}}
}

func (h *handler) SendMessage(ctx context.Context, req *sdk.SendMessageRequest) (sdk.SendMessageResult, error) {
	s := h.s
	snd, ok := senderFrom(ctx)
	if !ok {
		return nil, sdk.ErrUnauthenticated
	}
	if req.Message == nil {
		return nil, badMessage("no message")
	}
	if c, ok, err := claimOf(req.Message); err != nil {
		return nil, err
	} else if ok {
		return s.claim(ctx, snd, c)
	}
	who := principal(snd)
	wait := req.Config == nil || !req.Config.ReturnImmediately
	if wait {
		if !s.enterWait() {
			return nil, rejection("", ReasonTooManyWaits)
		}
		defer s.leaveWait()
	}

	m, err := decode(req.Message)
	if err != nil {
		return nil, err
	}
	clientID := req.Message.ID
	if clientID != m.MessageID && clientID != "" {
		m.MessageID = s.tasks.alias(who, clientID, m.MessageID)
	}
	wire := wireCopy(req.Message, m.MessageID, clientID)

	if req.Message.TaskID != "" {
		return h.continuation(ctx, snd, m, wire, wait)
	}
	if m.ContextID == "" {
		m.ContextID = m.MessageID
		wire.ContextID = m.ContextID
	}
	if textBytes(m) > s.cfg.Limits.MaxMessageBytes {
		return nil, rejection(m.MessageID, receiver.ReasonTooLarge)
	}
	t, existing, err := s.tasks.create(who, m, wire, s.cfg.Limits.MaxOpenAsks, s.cfg.Limits.MaxPending)
	if err != nil {
		return nil, err
	}
	submitted := s.tasks.snapshot(t, nil)
	if !existing {
		if !s.start(t, receiver.Inbound{Ingress: receiver.A2A, Message: m, Sender: snd}, false) {
			s.tasks.remove(t)
			return nil, sdk.NewError(sdk.ErrServerError, "server closing")
		}
	}
	if !wait {
		if existing {
			return s.tasks.snapshot(t, nil), nil
		}
		return submitted, nil
	}
	if err := s.await(ctx, t); err != nil {
		return nil, err
	}
	final := s.tasks.snapshot(t, nil)
	if reason := taskReason(final); !existing && final.Status.State == sdk.TaskStateFailed && admissionReasons[reason] {
		s.tasks.remove(t)
		return nil, rejection(t.messageID, reason)
	}
	return final, nil
}

func taskReason(t *sdk.Task) string {
	ct, _ := t.Metadata[MetaKey].(map[string]any)
	r, _ := ct["reason"].(string)
	return r
}

func wireCopy(am *sdk.Message, messageID, clientID string) *sdk.Message {
	w := *am
	w.ID = messageID
	w.Parts = slices.Clone(am.Parts)
	w.Metadata = maps.Clone(am.Metadata)
	if clientID != messageID && clientID != "" {
		if w.Metadata == nil {
			w.Metadata = map[string]any{}
		}
		ct := map[string]any{}
		if old, ok := w.Metadata[MetaKey].(map[string]any); ok {
			ct = maps.Clone(old)
		}
		ct["client_message_id"] = clientID
		w.Metadata[MetaKey] = ct
	}
	return &w
}

func (s *Server) enterWait() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.waiting >= s.cfg.Limits.MaxWaiting {
		return false
	}
	s.waiting++
	return true
}

func (s *Server) leaveWait() {
	s.mu.Lock()
	s.waiting--
	s.mu.Unlock()
}

// await blocks until t is terminal; there is no server timeout (§5 Waiting).
func (s *Server) await(ctx context.Context, t *task) error {
	select {
	case <-t.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.closed:
		return sdk.NewError(sdk.ErrServerError, "server closing")
	}
}

func (s *Server) start(t *task, in receiver.Inbound, resume bool) bool {
	s.mu.Lock()
	select {
	case <-s.closed:
		s.mu.Unlock()
		return false
	default:
	}
	s.wg.Add(1)
	s.mu.Unlock()
	if !resume {
		s.armDeadline(t)
	}
	go func() {
		defer s.wg.Done()
		if s.beforeReceive != nil {
			s.beforeReceive(t.messageID)
		}
		recv := s.cfg.Receiver.Receive
		if rs, ok := s.cfg.Receiver.(Resumer); ok && resume {
			recv = rs.Resume
		}
		s.apply(t, recv(s.base, in))
	}()
	return true
}

func (s *Server) armDeadline(t *task) {
	if t.deadline.IsZero() {
		return
	}
	expire := func() {
		if done, _ := s.tasks.update(t, sdk.TaskStateFailed, "", ReasonDeadline, failedStatus(t, "failed", ReasonDeadline)); done {
			s.askClosed(t)
		}
	}
	if d := time.Until(t.deadline); d > 0 {
		s.tasks.setTimer(t, d, expire)
	} else {
		expire()
	}
}

func (s *Server) apply(t *task, res receiver.Result) {
	state := outcome(t.kind, res.State)
	delivery, reason := string(res.State), res.Reason
	if state == sdk.TaskStateFailed && reason == "" {
		reason = string(res.State)
	}
	if res.State == adapter.Rejected && res.Reason == receiver.ReasonCanceled {
		state, delivery, reason = sdk.TaskStateCanceled, journal.StateCanceled, ""
	}
	var status *sdk.Message
	if state == sdk.TaskStateFailed {
		status = failedStatus(t, res.State, reason)
	}
	done, err := s.tasks.update(t, state, delivery, reason, status)
	if err != nil {
		s.tasks.markUnknown(t)
		return
	}
	if done && state.Terminal() {
		s.askClosed(t)
	}
}

// askClosed closes the routes of a terminal ask task; it must run without
// the task store's lock held.
func (s *Server) askClosed(t *task) {
	if t.kind != msg.Ask || s.cfg.OnAskClosed == nil {
		return
	}
	s.cfg.OnAskClosed(s.tasks.messageIDs(t))
}

func (h *handler) continuation(ctx context.Context, snd receiver.Sender, m msg.Message, wire *sdk.Message, wait bool) (sdk.SendMessageResult, error) {
	s := h.s
	t, ok := s.tasks.lookup(wire.TaskID, principal(snd))
	if !ok {
		return nil, sdk.ErrTaskNotFound
	}
	s.tasks.mu.Lock()
	terminal, ctxID := t.state.Terminal(), t.contextID
	s.tasks.mu.Unlock()
	if terminal {
		return nil, sdk.ErrUnsupportedOperation
	}
	if m.ContextID != "" && m.ContextID != ctxID {
		return nil, badMessage("contextId does not match the task")
	}
	m.ContextID, wire.ContextID = ctxID, ctxID
	tid := string(t.id)
	m.TaskID = &tid

	res := s.cfg.Receiver.Receive(s.base, receiver.Inbound{Ingress: receiver.A2A, Message: m, Sender: snd})
	if res.State == adapter.Rejected {
		return nil, rejection(res.MessageID, res.Reason)
	}
	ct := map[string]any{}
	if old, ok := wire.Metadata[MetaKey].(map[string]any); ok {
		ct = maps.Clone(old)
	}
	ct["state"] = string(res.State)
	if wire.Metadata == nil {
		wire.Metadata = map[string]any{}
	}
	wire.Metadata[MetaKey] = ct
	if !res.Duplicate {
		if err := s.tasks.appendContinuation(t, wire); err != nil {
			if errors.Is(err, errBadMessage) {
				return nil, err
			}
			return nil, sdk.NewError(sdk.ErrInternalError, "message could not be recorded")
		}
	}
	if wait {
		if err := s.await(ctx, t); err != nil {
			return nil, err
		}
	}
	return s.tasks.snapshot(t, nil), nil
}

func (h *handler) GetTask(ctx context.Context, req *sdk.GetTaskRequest) (*sdk.Task, error) {
	snd, ok := senderFrom(ctx)
	if !ok {
		return nil, sdk.ErrUnauthenticated
	}
	t, ok := h.s.tasks.lookup(req.ID, principal(snd))
	if !ok {
		return nil, sdk.ErrTaskNotFound
	}
	return h.s.tasks.snapshot(t, req.HistoryLength), nil
}

func (h *handler) ListTasks(ctx context.Context, req *sdk.ListTasksRequest) (*sdk.ListTasksResponse, error) {
	snd, ok := senderFrom(ctx)
	if !ok {
		return nil, sdk.ErrUnauthenticated
	}
	return h.s.tasks.list(principal(snd), req)
}

// CancelTask succeeds only while no dispatch claim exists, so a canceled
// task never reached the harness (§5 C5).
func (h *handler) CancelTask(ctx context.Context, req *sdk.CancelTaskRequest) (*sdk.Task, error) {
	s := h.s
	snd, ok := senderFrom(ctx)
	if !ok {
		return nil, sdk.ErrUnauthenticated
	}
	t, ok := s.tasks.lookup(req.ID, principal(snd))
	if !ok {
		return nil, sdk.ErrTaskNotFound
	}
	s.tasks.mu.Lock()
	state := t.state
	s.tasks.mu.Unlock()
	if state != sdk.TaskStateSubmitted {
		return nil, sdk.ErrTaskNotCancelable
	}
	won, err := s.cfg.Receiver.Cancel(t.messageID)
	if err != nil {
		return nil, sdk.NewError(sdk.ErrInternalError, "cancel could not be recorded")
	}
	if !won {
		s.tasks.markDispatching(t)
		return nil, sdk.ErrTaskNotCancelable
	}
	ok, err = s.tasks.update(t, sdk.TaskStateCanceled, journal.StateCanceled, "", nil)
	if err != nil {
		return nil, sdk.NewError(sdk.ErrInternalError, "cancel could not be recorded")
	}
	if !ok {
		return nil, sdk.ErrTaskNotCancelable
	}
	s.askClosed(t)
	return s.tasks.snapshot(t, nil), nil
}

func textBytes(m msg.Message) int {
	n := 0
	for _, p := range m.Parts {
		n += len(p.Text)
	}
	return n
}
