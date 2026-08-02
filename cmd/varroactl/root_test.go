package main

import (
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// saveRootRegistrars returns a copy of the current rootRegistrars.
func saveRootRegistrars() []rootRegistrar {
	saved := make([]rootRegistrar, len(rootRegistrars))
	copy(saved, rootRegistrars)
	return saved
}

// restoreRootRegistrars restores rootRegistrars to a previously saved state.
func restoreRootRegistrars(saved []rootRegistrar) {
	rootRegistrars = saved
}

// TestRootRegistrar_AddCommand verifies a callback can add a new top-level
// command and that command executes. This test isolates registrars.
func TestRootRegistrar_AddCommand(t *testing.T) {
	saved := saveRootRegistrars()
	rootRegistrars = nil
	t.Cleanup(func() { restoreRootRegistrars(saved) })

	registerRootCommand(func(root *cobra.Command) {
		root.AddCommand(&cobra.Command{
			Use: "test-add",
			RunE: func(cmd *cobra.Command, args []string) error {
				return nil
			},
		})
	})

	root := newRootCmd()
	root.SetArgs([]string{"test-add"})
	err := root.Execute()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := false
	for _, c := range root.Commands() {
		if c.Name() == "test-add" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("test-add command not found in root.Commands()")
	}
}

// TestRootRegistrar_LocateExisting verifies a registrar can locate an existing
// command (logs) and add a flag to it, proving post-assembly invocation.
func TestRootRegistrar_LocateExisting(t *testing.T) {
	saved := saveRootRegistrars()
	rootRegistrars = nil
	t.Cleanup(func() { restoreRootRegistrars(saved) })

	registerRootCommand(func(root *cobra.Command) {
		for _, c := range root.Commands() {
			if c.Name() == "logs" {
				c.Flags().Bool("follow", false, "follow logs")
				break
			}
		}
	})

	root := newRootCmd()
	root.SetArgs([]string{"logs", "ns/name", "--follow"})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected error (logs not implemented)")
	}
	if !strings.Contains(err.Error(), "not implemented yet") {
		t.Fatalf("expected 'not implemented yet', got: %v", err)
	}

	logsCmd, _, _ := root.Find([]string{"logs"})
	if logsCmd == nil {
		t.Fatal("logs command not found")
	}
	f := logsCmd.Flag("follow")
	if f == nil {
		t.Fatal("expected --follow flag to exist on logs command")
	}
}

// TestRootRegistrar_CommandNames verifies the exact set of top-level command
// names the C2 framework itself defines. Per the C2 spec the assertion runs
// with rootRegistrars empty: init()-registered callbacks (C2 action-verb
// noun attachments, C3 top-level verbs) are saved and restored around the
// build so the list stays stable as later changes register more commands.
func TestRootRegistrar_CommandNames(t *testing.T) {
	saved := rootRegistrars
	rootRegistrars = nil
	t.Cleanup(func() { rootRegistrars = saved })
	root := newRootCmd()
	cmds := root.Commands()

	names := make([]string, len(cmds))
	for i, c := range cmds {
		names[i] = c.Name()
	}
	sort.Strings(names)

	// Only commands newRootCmd attaches directly: the CRUD verb parents, the
	// action verb parents, logs, and config. Everything else (login, logout,
	// whoami, version, and all C3 verbs) attaches via registerRootCommand and
	// is excluded here by design.
	expected := []string{
		"approve",
		"config",
		"create",
		"delete",
		"describe",
		"diff",
		"edit",
		"get",
		"logs",
		"patch",
		"power",
		"preflight",
		"preview",
		"reconcile",
		"render",
		"reprovision",
		"restart",
	}
	sort.Strings(expected)

	if len(names) != len(expected) {
		t.Fatalf("expected %d commands, got %d:\n  got:      %v\n  expected: %v",
			len(expected), len(names), names, expected)
	}
	for i, n := range names {
		if n != expected[i] {
			t.Fatalf("command mismatch at position %d: got %q, expected %q\n  got:      %v\n  expected: %v",
				i, n, expected[i], names, expected)
		}
	}
}
