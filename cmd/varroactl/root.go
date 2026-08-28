package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// ---------------------------------------------------------------------------
// Exit-code mapping
// ---------------------------------------------------------------------------

type usageError struct{ error }

func usagef(format string, args ...interface{}) error {
	return usageError{fmt.Errorf(format, args...)}
}

// validOutputFormats for -o validation.
var validOutputFormats = map[string]bool{
	"table": true,
	"wide":  true,
	"json":  true,
	"yaml":  true,
	"name":  true,
}

// ---------------------------------------------------------------------------
// Verb parents
// ---------------------------------------------------------------------------

// verbCommands holds the parent cobra commands for each verb group.
type verbCommands struct {
	get, describe, create, delete, edit, patch *cobra.Command
}

// actionVerbs holds the action-verb parent commands (registered directly on root).
type actionVerbs struct {
	restart, reprovision, reconcile, approve, diff, preflight, render, preview, power, hibernate, wake *cobra.Command
}

// ---------------------------------------------------------------------------
// Noun registry – C3 attaches nouns without touching C2 files
// ---------------------------------------------------------------------------

type nounRegistrar func(v *verbCommands)

var nounRegistrars []nounRegistrar

func registerNoun(r nounRegistrar) {
	nounRegistrars = append(nounRegistrars, r)
}

// ---------------------------------------------------------------------------
// Root-command extension seam (C3-1, coordinator arbitration)
// ---------------------------------------------------------------------------

type rootRegistrar func(root *cobra.Command)

var rootRegistrars []rootRegistrar

func registerRootCommand(r rootRegistrar) {
	rootRegistrars = append(rootRegistrars, r)
}

// ---------------------------------------------------------------------------
// run – top-level entrypoint called from main()
// ---------------------------------------------------------------------------

func run(args []string) int {
	rootCmd := newRootCmd()
	rootCmd.SetArgs(args)

	err := rootCmd.Execute()
	if err == nil {
		return 0
	}

	// usageError (including cobra parse/unknown-command errors) → exit 2
	var ue usageError
	if errorsAs(err, &ue) {
		fmt.Fprintln(os.Stderr, "Error:", ue.error)
		return 2
	}
	// cobra's SilenceErrors is on; but if cobra itself returned a parse
	// error before any PersistentPreRunE ran, it will be a "unknown flag"
	// style error that we can check.
	if isUsageError(err) {
		fmt.Fprintln(os.Stderr, "Error:", err)
		return 2
	}

	fmt.Fprintln(os.Stderr, "Error:", err)
	return 1
}

// isUsageError returns true for cobra flag-parsing and unknown-command errors.
func isUsageError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "unknown flag") ||
		strings.Contains(msg, "unknown command") ||
		strings.Contains(msg, "flag needs an argument") ||
		strings.Contains(msg, "flag accessed but not defined") ||
		strings.Contains(msg, "bad flag syntax") ||
		strings.HasPrefix(msg, "invalid argument") ||
		strings.HasPrefix(msg, "required flag(s)")
}

// errorsAs is a small wrapper so we don't import errors just for this.
func errorsAs(err error, target interface{}) bool {
	// Simple unwrap-and-check for usageError.
	for {
		if ue, ok := err.(usageError); ok {
			*(target.(*usageError)) = ue
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
}

// ---------------------------------------------------------------------------
// newRootCmd – builds the full command tree
// ---------------------------------------------------------------------------

func newRootCmd() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:           "varroactl",
		Short:         "CLI for Varroa Jenkins management",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// Persistent flags (design §2)
	rootCmd.PersistentFlags().String("context", "", "context name")
	rootCmd.PersistentFlags().String("server", "", "server URL")
	rootCmd.PersistentFlags().StringP("namespace", "n", "", "namespace")
	rootCmd.PersistentFlags().BoolP("all-namespaces", "A", false, "all namespaces")
	rootCmd.PersistentFlags().StringP("output", "o", "table", "output format: table|wide|json|yaml|name")
	rootCmd.PersistentFlags().Bool("no-headers", false, "suppress table headers")

	// PersistentPreRunE: validate -o and -n/-A exclusivity
	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		// Skip validation for subcommands that are not making API requests.
		// We always validate -o and -n/-A so enforcement is consistent.
		o, _ := cmd.Flags().GetString("output")
		if o != "" && !validOutputFormats[o] {
			return usagef("invalid output format %q: must be table, wide, json, yaml, or name", o)
		}
		n, _ := cmd.Flags().GetString("namespace")
		a, _ := cmd.Flags().GetBool("all-namespaces")
		if n != "" && a {
			return usagef("--namespace / -n and --all-namespaces / -A are mutually exclusive")
		}
		return nil
	}

	// Verb parents
	verbs := &verbCommands{
		get:      newVerbParent("get", "Get resources"),
		describe: newVerbParent("describe", "Describe resources"),
		create:   newVerbParent("create", "Create resources"),
		delete:   newVerbParent("delete", "Delete resources"),
		edit:     newVerbParent("edit", "Edit resources"),
		patch:    newVerbParent("patch", "Patch resources"),
	}
	rootCmd.AddCommand(verbs.get, verbs.describe, verbs.create, verbs.delete, verbs.edit, verbs.patch)

	// Noun registrars attach noun subcommands to the verb parents.
	for _, r := range nounRegistrars {
		r(verbs)
	}

	// Action verb parents (registered directly on root)
	actions := &actionVerbs{
		restart:     newVerbParent("restart", "Restart a resource"),
		reprovision: newVerbParent("reprovision", "Reprovision a resource"),
		reconcile:   newVerbParent("reconcile", "Reconcile a resource"),
		approve:     newVerbParent("approve", "Approve a resource"),
		diff:        newVerbParent("diff", "Diff a resource"),
		preflight:   newVerbParent("preflight", "Preflight a resource"),
		render:      newVerbParent("render", "Render a resource"),
		preview:     newVerbParent("preview", "Preview a resource"),
		power:       newVerbParent("power", "Set power state of a resource"),
		hibernate:   newVerbParent("hibernate", "Hibernate a resource"),
		wake:        newVerbParent("wake", "Wake a resource"),
	}
	rootCmd.AddCommand(actions.restart, actions.reprovision, actions.reconcile,
		actions.approve, actions.diff, actions.preflight, actions.render, actions.preview, actions.power,
		actions.hibernate, actions.wake)

	// Top-level logs command (deliberate grammar exception – design §8)
	logsCmd := &cobra.Command{
		Use:   "logs NS/NAME",
		Short: "Show controller logs",
		Long: `Show controller logs.

Accepts NS/NAME or "controller NS/NAME" argument forms.
One-shot GET; does not follow.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("not implemented yet")
		},
	}
	rootCmd.AddCommand(logsCmd)

	// Config command (section 2)
	addConfigCommand(rootCmd)

	// Root-command extension seam – runs AFTER the full tree is assembled.
	for _, r := range rootRegistrars {
		r(rootCmd)
	}

	return rootCmd
}

// newVerbParent creates a cobra command that acts as a verb parent (no RunE).
// Not setting RunE lets cobra show subcommands in help.
func newVerbParent(use, short string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
	}
}
