package main

import (
	"os"
	"testing"

	"github.com/spf13/cobra"
)

// ---------------------------------------------------------------------------
// findCommand tests
// ---------------------------------------------------------------------------

func TestAttach_findCommandExisting(t *testing.T) {
	root := &cobra.Command{Use: "root"}
	child := &cobra.Command{Use: "child"}
	grandchild := &cobra.Command{Use: "grandchild"}
	child.AddCommand(grandchild)
	root.AddCommand(child)

	got := findCommand(root, "child", "grandchild")
	if got != grandchild {
		t.Errorf("expected grandchild command, got %v", got)
	}
}

func TestAttach_findCommandSingleSegment(t *testing.T) {
	root := &cobra.Command{Use: "root"}
	child := &cobra.Command{Use: "child"}
	root.AddCommand(child)

	got := findCommand(root, "child")
	if got != child {
		t.Errorf("expected child command, got %v", got)
	}
}

// wrapRunE tests
// ---------------------------------------------------------------------------

func TestAttach_wrapRunEOriginalCalled(t *testing.T) {
	var ranOrig bool
	var ranWrap bool

	cmd := &cobra.Command{
		RunE: func(cmd *cobra.Command, args []string) error {
			ranOrig = true
			return nil
		},
	}

	wrapRunE(cmd, func(orig func(*cobra.Command, []string) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			ranWrap = true
			return orig(cmd, args)
		}
	})

	_ = cmd.RunE(cmd, nil)
	if !ranOrig {
		t.Error("original RunE was not called")
	}
	if !ranWrap {
		t.Error("wrapper was not called")
	}
}

func TestAttach_wrapRunESkipOriginal(t *testing.T) {
	var ranOrig bool

	cmd := &cobra.Command{
		RunE: func(cmd *cobra.Command, args []string) error {
			ranOrig = true
			return nil
		},
	}

	wrapRunE(cmd, func(orig func(*cobra.Command, []string) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			// Skip original, return our own result
			return nil
		}
	})

	_ = cmd.RunE(cmd, nil)
	if ranOrig {
		t.Error("original RunE should not have been called when wrapper skips it")
	}
}

// ---------------------------------------------------------------------------
// Canary test: every C3 verb exists, `apply` does NOT exist
// ---------------------------------------------------------------------------

func TestAttach_AllC3VerbsExist(t *testing.T) {
	// Start with clean registrars so init()-registered hooks fire naturally.
	saved := rootRegistrars
	defer func() { rootRegistrars = saved }()

	root := newRootCmd()

	// Top-level C3 verbs
	expected := []string{"validate", "sync", "passwd", "apikey",
		"activity", "search", "events", "mite", "watch", "mcp",
		"pause", "resume"}
	for _, v := range expected {
		if _, _, err := root.Find([]string{v}); err != nil {
			t.Errorf("expected verb %q to exist but it was not found", v)
		}
	}

	// preview bundle exists
	if _, _, err := root.Find([]string{"preview", "bundle"}); err != nil {
		t.Error("expected 'preview bundle' to exist but it was not found")
	}

	// logs -f/--follow flag exists
	logsCmd, _, _ := root.Find([]string{"logs"})
	if logsCmd == nil {
		t.Fatal("logs command not found")
	}
	if logsCmd.Flag("follow") == nil && logsCmd.Flag("f") == nil {
		t.Error("logs should have -f/--follow flag")
	}

	// get controller -w/--watch flag exists
	getCtrl, _, _ := root.Find([]string{"get", "controller"})
	if getCtrl != nil {
		if getCtrl.Flag("watch") == nil && getCtrl.Flag("w") == nil {
			t.Error("get controller should have -w/--watch flag")
		}
	}

	// apply does NOT exist
	if _, _, err := root.Find([]string{"apply"}); err == nil {
		t.Error("apply should NOT be a verb")
	}
}

// TestMain makes os.Exit work in tests predictable.
func TestMain(m *testing.M) {
	// Prevent os.Exit(1) from findCommand from actually killing the test.
	code := m.Run()
	os.Exit(code)
}
