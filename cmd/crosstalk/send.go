package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/registry"
	"github.com/GoodOlClint/crosstalk/internal/udsingress"
)

type sendAck struct {
	MessageID string `json:"message_id"`
	State     string `json:"state"`
	Reason    string `json:"reason"`
}

func pickTarget(ds []registry.Descriptor, to string) (registry.Descriptor, error) {
	var hit []registry.Descriptor
	for _, d := range ds {
		if d.AgentID == to || d.Name == to {
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

// sendTo writes the uds auth line and one user line, so the message enters
// the target's receiver chain as an unverified inform.
func sendTo(d registry.Descriptor, text string) (sendAck, error) {
	var ack sendAck
	raw, err := os.ReadFile(udsingress.KeyPath(d.UDSPath))
	if err != nil {
		return ack, err
	}
	key := strings.TrimSpace(string(raw))
	c, err := net.DialTimeout("unix", d.UDSPath, 3*time.Second)
	if err != nil {
		return ack, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(15 * time.Second))
	auth, _ := json.Marshal(map[string]string{"type": "auth", "token": key})
	user, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]string{"role": "user", "content": text}})
	if _, err := c.Write(append(append(auth, '\n'), append(user, '\n')...)); err != nil {
		return ack, err
	}
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return ack, errors.New("no answer from the agent; its token or socket was refused")
	}
	return ack, json.Unmarshal(line, &ack)
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
