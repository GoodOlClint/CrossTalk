package udsingress

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// Ack is the ingress's answer to an authenticated user line.
type Ack struct {
	MessageID string `json:"message_id"`
	State     string `json:"state"`
	Reason    string `json:"reason"`
}

// Send writes the auth line and one user line to the agent socket at path,
// using the key file next to it, and returns the ack. It never creates a key.
func Send(path, text string, timeout time.Duration) (Ack, error) {
	var ack Ack
	user, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]string{"role": "user", "content": text}})
	if len(user) > DefaultMaxFrame {
		return ack, fmt.Errorf("message is %d bytes encoded; the limit is %d", len(user), DefaultMaxFrame)
	}
	raw, err := os.ReadFile(KeyPath(path))
	if err != nil {
		return ack, err
	}
	auth, _ := json.Marshal(map[string]string{"type": "auth", "token": strings.TrimSpace(string(raw))})
	c, err := net.DialTimeout("unix", path, 3*time.Second)
	if err != nil {
		return ack, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(timeout))
	if _, err := c.Write(append(append(auth, '\n'), append(user, '\n')...)); err != nil {
		return ack, err
	}
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return ack, errors.New("no answer from the agent: the message may still be delivered, or its token was refused; do not retry blindly, a retry can deliver it twice")
	}
	return ack, json.Unmarshal(line, &ack)
}
