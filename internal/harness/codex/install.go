package codex

import (
	"context"
	"os/exec"

	"github.com/GoodOlClint/crosstalk/internal/harness"
)

var _ harness.Installer = (*Harness)(nil)

func (h *Harness) Found() bool {
	_, err := exec.LookPath("codex")
	return err == nil
}

func (h *Harness) InstallSteps(env harness.InstallEnv) []harness.Step { return nil }

func (h *Harness) UninstallSteps(env harness.InstallEnv) []harness.Step { return nil }

func (h *Harness) Checks(ctx context.Context, env harness.InstallEnv) []harness.CheckResult {
	return nil
}
