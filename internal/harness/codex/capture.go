package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver"
)

// CaptureFile is the JSONL file Capture writes in its output directory.
const CaptureFile = "frames.jsonl"

// BogusTurnID never names a real turn, so a steer with it must fail its
// expectedTurnId precondition and deliver nothing.
const BogusTurnID = "00000000-0000-7000-8000-000000000000"

// ProbeText is the input of the precondition-failing steer.
const ProbeText = "crosstalk capture probe: expectedTurnId is deliberately wrong; this text must not be delivered"

// Capture step labels.
const (
	StepInitialize    = "initialize"
	StepLoadedList    = "thread_loaded_list"
	StepRead          = "thread_read"
	StepTurns         = "thread_turns_list"
	StepSteerMismatch = "steer_mismatch"
	StepSteerNoActive = "steer_no_active_turn"
	StepSteerLive     = "steer_live"
	StepMcpStatus     = "mcp_server_status"
	StepListen        = "listen"
	StepNotLoadedSkip = "thread_not_loaded"
	StepLiveSkipped   = "steer_live_skipped"
	CaptureDirOut     = appserver.DirOut
	CaptureDirIn      = appserver.DirIn
	CaptureKindFrame  = "frame"
	CaptureKindMeta   = "meta"
	CaptureKindNote   = "note"
)

type CaptureOptions struct {
	Endpoint string
	ThreadID string
	OutDir   string
	// SteerText, when non-empty, is steered into the thread's real active
	// turn (step steer_live). It is delivered if the turn is steerable; use
	// it while /review or /compact runs to record activeTurnNotSteerable.
	SteerText string
	// Listen keeps the connection open this long at the end to record
	// notifications.
	Listen time.Duration
}

// CaptureRecord is one JSONL line.
type CaptureRecord struct {
	Seq   int             `json:"seq"`
	Step  string          `json:"step"`
	Kind  string          `json:"kind"`
	Dir   string          `json:"dir,omitempty"`
	Frame json.RawMessage `json:"frame,omitempty"`
	Path  string          `json:"path,omitempty"`
	Meta  json.RawMessage `json:"meta,omitempty"`
	Note  string          `json:"note,omitempty"`
}

type recorder struct {
	mu   sync.Mutex
	w    *bufio.Writer
	seq  int
	step string
	err  error
}

func (r *recorder) setStep(s string) {
	r.mu.Lock()
	r.step = s
	r.mu.Unlock()
}

func (r *recorder) emitLocked(rec CaptureRecord) {
	r.seq++
	rec.Seq, rec.Step = r.seq, r.step
	b, err := json.Marshal(rec)
	if err == nil {
		_, err = r.w.Write(append(b, '\n'))
	}
	if err != nil && r.err == nil {
		r.err = err
	}
}

func (r *recorder) note(step, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.step = step
	r.emitLocked(CaptureRecord{Kind: CaptureKindNote, Note: text})
}

func (r *recorder) frame(dir string, raw []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	frame := json.RawMessage(slices.Clone(raw))
	if !json.Valid(frame) {
		b, _ := json.Marshal(string(raw))
		frame = b
	}
	r.emitLocked(CaptureRecord{Kind: CaptureKindFrame, Dir: dir, Frame: frame})
	if dir != appserver.DirIn {
		return
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return
	}
	for _, m := range findMeta(v, "$") {
		r.emitLocked(CaptureRecord{Kind: CaptureKindMeta, Dir: dir, Path: m.path, Meta: m.raw})
	}
}

type metaHit struct {
	path string
	raw  json.RawMessage
}

func findMeta(v any, path string) []metaHit {
	var out []metaHit
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := path + "." + k
			if k == "_meta" && t[k] != nil {
				b, _ := json.Marshal(t[k])
				out = append(out, metaHit{p, b})
			}
			out = append(out, findMeta(t[k], p)...)
		}
	case []any:
		for i, c := range t {
			out = append(out, findMeta(c, fmt.Sprintf("%s[%d]", path, i))...)
		}
	}
	return out
}

// Capture records raw app-server frames for one thread into
// OutDir/frames.jsonl (§6 V11): the handshake, thread/loaded/list,
// thread/read, thread/turns/list, a turn/steer whose expectedTurnId is
// BogusTurnID (the mismatch or no-active-turn error case, which delivers
// nothing), an optional real steer (SteerText), mcpServerStatus/list, and
// every _meta object seen in inbound frames. Steers are sent only when the
// thread is in this endpoint's thread/loaded/list.
func Capture(ctx context.Context, opt CaptureOptions) (string, error) {
	if opt.ThreadID == "" || opt.OutDir == "" || opt.Endpoint == "" {
		return "", errors.New("capture: endpoint, thread and output directory are required")
	}
	if err := os.MkdirAll(opt.OutDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(opt.OutDir, CaptureFile)
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	rec := &recorder{w: bufio.NewWriter(f)}
	err = capture(ctx, opt, rec)
	rec.mu.Lock()
	if ferr := rec.w.Flush(); ferr != nil && rec.err == nil {
		rec.err = ferr
	}
	werr := rec.err
	rec.mu.Unlock()
	return path, errors.Join(err, werr, f.Sync())
}

func capture(ctx context.Context, opt CaptureOptions, rec *recorder) error {
	rec.setStep(StepInitialize)
	c, err := appserver.Dial(ctx, opt.Endpoint, appserver.Options{OnFrame: rec.frame})
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.Initialize(ctx, appserver.ClientInfo{Name: "crosstalk-capture", Version: "1"}); err != nil {
		return err
	}
	rec.setStep(StepLoadedList)
	ids, err := c.LoadedThreads(ctx)
	if err != nil {
		return err
	}
	rec.setStep(StepRead)
	_, _ = c.ReadThread(ctx, opt.ThreadID)
	rec.setStep(StepTurns)
	active, _ := c.ActiveTurn(ctx, opt.ThreadID)
	if !slices.Contains(ids, opt.ThreadID) {
		rec.note(StepNotLoadedSkip, "thread is not in thread/loaded/list on this endpoint; no turn/steer sent")
	} else {
		step := StepSteerMismatch
		if active == "" {
			step = StepSteerNoActive
		}
		rec.setStep(step)
		_, _ = c.SteerTurn(ctx, opt.ThreadID, BogusTurnID, ProbeText)
		switch {
		case opt.SteerText != "" && active != "":
			rec.setStep(StepSteerLive)
			_, _ = c.SteerTurn(ctx, opt.ThreadID, active, opt.SteerText)
		case opt.SteerText != "":
			rec.note(StepLiveSkipped, "no inProgress turn; live steer not sent")
		}
	}
	rec.setStep(StepMcpStatus)
	_, _ = c.McpServerStatus(ctx, opt.ThreadID, "")
	if opt.Listen > 0 {
		rec.setStep(StepListen)
		t := time.NewTimer(opt.Listen)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
		case <-c.Done():
		}
	}
	return nil
}

// ReadCapture parses a frames.jsonl file.
func ReadCapture(path string) ([]CaptureRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []CaptureRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), appserver.MaxMessageSize)
	for sc.Scan() {
		var r CaptureRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, len(out)+1, err)
		}
		out = append(out, r)
	}
	return out, sc.Err()
}
