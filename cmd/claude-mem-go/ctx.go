package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Hook budgets: each is a little under the matching "timeout" in
// hooks/hooks.json so that a stuck store query yields a clean, empty hook
// reply from this process rather than Claude Code killing it mid-write.
// hookwiring_test.go pins these against the manifest.
const (
	sessionStartBudget  = 8 * time.Second   // hooks.json: context, timeout 10
	promptContextBudget = 25 * time.Second  // hooks.json: prompt-context, timeout 30
	fileContextBudget   = 8 * time.Second   // hooks.json: file-context, timeout 10
	stopBudget          = 110 * time.Second // hooks.json: stop, timeout 120
)

// hookContext bounds one hook invocation's work to budget.
func hookContext(budget time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), budget)
}

// cliContext is for operator-run subcommands (search, export, prune, …):
// unbounded, but Ctrl-C cancels the in-flight query instead of leaving it
// to finish against a store nobody is waiting on any more.
func cliContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
