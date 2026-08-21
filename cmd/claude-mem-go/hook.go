package main

import (
	"flag"
	"os"

	"claude-mem-go/hook"
	"claude-mem-go/worker"
)

func cmdHook(args []string) int {
	fs := flag.NewFlagSet("hook", flag.ExitOnError)
	socketPath := fs.String("socket", worker.DefaultSocketPath(), "unix socket the worker daemon listens on")
	fs.Parse(args)

	l := openLog("hook.log")
	n, err := hook.Forward(*socketPath, os.Stdin)
	if err != nil {
		// A missing/unreachable daemon must never surface as a Claude
		// Code-visible hook failure — log loudly, exit clean.
		l.Errorf("FAILED forwarding to worker at %s: %v (is `claude-mem-go worker` running?)", *socketPath, err)
		return 0
	}
	l.Debugf("forwarded %d bytes to worker, exiting immediately", n)
	return 0
}
