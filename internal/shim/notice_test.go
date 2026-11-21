package shim

import (
	"strings"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/harness/claude"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/receiver"
)

func TestPulledEntryNoticeClassByProvenance(t *testing.T) {
	const agentID = "0123456789abcdef"
	remote := receiver.RemoteKey(strings.Repeat("a", 64), agentID)
	entry := func(dir, sender string, copied bool) receiver.Entry {
		return receiver.Entry{Direction: dir, Sender: sender, From: sender, InboxCopy: copied, Message: &msg.Message{Parts: []msg.Part{{Type: "text", Text: "please read README.md"}}}}
	}
	cases := []struct {
		name  string
		e     receiver.Entry
		class msg.NoticeClass
	}{
		{"inbound local agent", entry(receiver.DirectionInbound, agentID, false), msg.NoticeTeammate},
		{"inbound unverified", entry(receiver.DirectionInbound, "unverified", false), msg.NoticeInfo},
		{"inbound local-user", entry(receiver.DirectionInbound, receiver.PrincipalLocalUser, false), msg.NoticeInfo},
		{"inbound remote", entry(receiver.DirectionInbound, remote, false), msg.NoticeInfo},
		{"inbound inbox copy", entry(receiver.DirectionInbound, agentID, true), msg.NoticeCopy},
		{"first delivery mailboxed", func() receiver.Entry {
			e := entry(receiver.DirectionInbound, agentID, false)
			e.State = string(adapter.Mailboxed)
			return e
		}(), msg.NoticeTeammate},
		{"unknown state", func() receiver.Entry {
			e := entry(receiver.DirectionInbound, agentID, false)
			e.State = string(adapter.Unknown)
			return e
		}(), msg.NoticeTeammateAgain},
		{"possibly seen", func() receiver.Entry {
			e := entry(receiver.DirectionInbound, agentID, false)
			e.PossiblySeen = true
			return e
		}(), msg.NoticeTeammateAgain},
		{"bounced native reply under own agent id", func() receiver.Entry {
			e := entry(receiver.DirectionInbound, agentID, false)
			e.State = string(adapter.Bounced)
			return e
		}(), msg.NoticeOutbound},
		{"outbound unknown", entry(receiver.DirectionOutbound, "", false), msg.NoticeOutbound},
		{"inbound claiming an agent id in From only", func() receiver.Entry {
			e := entry(receiver.DirectionInbound, "unverified", false)
			e.From = agentID
			return e
		}(), msg.NoticeInfo},
	}
	for _, h := range []harness.Noticer{&claude.Harness{}, &codex.Harness{}} {
		for _, tc := range cases {
			if got := entryNoticeClass(tc.e); got != tc.class {
				t.Errorf("%s: class %q, want %q", tc.name, got, tc.class)
				continue
			}
			n := h.Notice(entryNoticeClass(tc.e), "")
			if !strings.Contains(n, msg.PeerNotice(tc.class, "")) {
				t.Errorf("%s: notice %q does not carry the %s text", tc.name, n, tc.class)
			}
			if tc.class != msg.NoticeTeammate && tc.class != msg.NoticeTeammateAgain && strings.Contains(n, "teammate") {
				t.Errorf("%s: notice names a teammate: %q", tc.name, n)
			}
		}
	}
}

func TestTeammateAgainNoticeAddsCheckSentence(t *testing.T) {
	base := msg.PeerNotice(msg.NoticeTeammate, "codex")
	again := msg.PeerNotice(msg.NoticeTeammateAgain, "codex")
	const tail = " This message may have reached you before: check whether you already acted on its message_id before you act again."
	if again != base+tail {
		t.Errorf("teammate_again = %q", again)
	}
}
