package registry

import "time"

const Protocol = 1

const (
	RolePrimary  = "primary"
	RoleSubagent = "subagent"
)

type Descriptor struct {
	AgentID          string            `json:"agent_id"`
	Name             string            `json:"name"`
	Harness          string            `json:"harness"`
	Role             string            `json:"role"`
	Owner            *string           `json:"owner"`
	PID              int               `json:"pid"`
	HarnessPID       *int              `json:"harness_pid"`
	HarnessStartedAt *time.Time        `json:"harness_started_at"`
	Cwd              string            `json:"cwd"`
	SessionRef       string            `json:"session_ref"`
	A2AURL           string            `json:"a2a_url"`
	UDSPath          string            `json:"uds_path"`
	InstanceID       string            `json:"instance_id"`
	HarnessEndpoint  *string           `json:"harness_endpoint"`
	HarnessVersion   *string           `json:"harness_version"`
	CredentialRef    string            `json:"credential_ref"`
	Installed        bool              `json:"installed"`
	StartedAt        time.Time         `json:"started_at"`
	Protocol         int               `json:"protocol"`
	Inboxes          []InboxMembership `json:"inboxes,omitempty"`
}

type InboxMembership struct {
	Name       string    `json:"name"`
	AttachedAt time.Time `json:"attached_at"`
}
