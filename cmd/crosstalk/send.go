package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
	"github.com/GoodOlClint/crosstalk/internal/udsingress"
)

// sendTimeout outlasts the Codex inject timeout (30s), the slowest delivery.
var sendTimeout = 45 * time.Second

type sendAck = udsingress.Ack

func pickTarget(ds []registry.Descriptor, to string) (registry.Descriptor, error) {
	id := mcpserver.IsAgentID(to)
	var hit []registry.Descriptor
	for _, d := range ds {
		if id && d.AgentID == to || !id && d.Name == to {
			hit = append(hit, d)
		}
	}
	switch len(hit) {
	case 1:
		return hit[0], nil
	case 0:
		return registry.Descriptor{}, fmt.Errorf("no CrossTalk agent named %q", to)
	}
	return registry.Descriptor{}, fmt.Errorf("several agents are named %q; use an agent_id", to)
}

func sendTo(d registry.Descriptor, text string) (sendAck, error) {
	return udsingress.Send(d.UDSPath, text, sendTimeout)
}

func runSend(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 2 {
		fmt.Fprintln(stderr, "usage: crosstalk send <name|agent_id> <text|->")
		return 2
	}
	text := args[1]
	if text == "-" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintf(stderr, "crosstalk send: %v\n", err)
			return 1
		}
		text = strings.TrimRight(string(b), "\n")
	}
	dir, err := registry.DefaultDir()
	if err == nil {
		var ds []registry.Descriptor
		if ds, err = registry.New(dir).List(); err == nil {
			var d registry.Descriptor
			if d, err = pickTarget(ds, args[0]); err == nil {
				var ack sendAck
				if ack, err = sendTo(d, text); err == nil {
					b, _ := json.Marshal(ack)
					fmt.Fprintln(stdout, string(b))
					if ack.State == "accepted" || ack.State == "queued" {
						return 0
					}
					return 1
				}
			}
		}
	}
	fmt.Fprintf(stderr, "crosstalk send: %v\n", err)
	return 1
}
