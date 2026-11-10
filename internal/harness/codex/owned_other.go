//go:build !darwin && !linux

package codex

import "os"

func ownedBySelf(os.FileInfo) bool { return false }
