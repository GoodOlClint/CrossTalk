package main

import (
	"io"
	"runtime"
)

var (
	unitRun          = installRun
	remoteGOOS       = runtime.GOOS
	remoteCandidates = func() []string { return nil }
)

func runRemoteEnable(args []string, in io.Reader, tty bool, stdout, stderr io.Writer) int {
	return 1
}

func runRemoteDisable(args []string, in io.Reader, tty bool, stdout, stderr io.Writer) int {
	return 1
}

func orderCandidates(addrs []string) []string { return nil }
