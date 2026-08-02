package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// findCommand walks the command tree matching each segment by cmd.Name().
// On miss, it prints a diagnostic to stderr and calls os.Exit(1).
func findCommand(root *cobra.Command, path ...string) *cobra.Command {
	current := root
	for _, segment := range path {
		found := false
		for _, child := range current.Commands() {
			if child.Name() == segment {
				current = child
				found = true
				break
			}
		}
		if !found {
			fmt.Fprintf(os.Stderr, "varroactl: internal error: attachment target %q not found (C2 framework changed)\n", segment)
			os.Exit(1)
		}
	}
	return current
}

// lookupCommand is findCommand's non-fatal sibling: it returns nil when a
// segment is missing instead of exiting. Use it when the target is a command
// this registrar may legitimately have to create itself — a NEW verb parent
// that no other registrar has added yet.
func lookupCommand(root *cobra.Command, path ...string) *cobra.Command {
	current := root
	for _, segment := range path {
		var next *cobra.Command
		for _, child := range current.Commands() {
			if child.Name() == segment {
				next = child
				break
			}
		}
		if next == nil {
			return nil
		}
		current = next
	}
	return current
}

// wrapRunE saves cmd's current RunE, then installs a new RunE produced by
// wrap. The wrapper receives the original RunE so it can delegate.
func wrapRunE(cmd *cobra.Command, wrap func(orig func(*cobra.Command, []string) error) func(*cobra.Command, []string) error) {
	orig := cmd.RunE
	cmd.RunE = wrap(orig)
}
