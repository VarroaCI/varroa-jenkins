package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Injectable password readers for testing.
var (
	readOldPasswordFn     = func() ([]byte, error) { return readWithPrompt("Current password: ") }
	readNewPasswordFn     = func() ([]byte, error) { return readWithPrompt("New password: ") }
	readConfirmPasswordFn = func() ([]byte, error) { return readWithPrompt("Confirm new password: ") }
)

func readWithPrompt(prompt string) ([]byte, error) {
	fmt.Fprint(os.Stderr, prompt)
	p, err := term.ReadPassword(int(os.Stdin.Fd()))
	if err != nil {
		return nil, err
	}
	fmt.Fprintln(os.Stderr)
	return p, nil
}

func init() {
	registerRootCommand(func(root *cobra.Command) {
		passwdCmd := &cobra.Command{
			Use:   "passwd [USER] [--password-stdin]",
			Short: "Change password",
			Long: `Change your own password, or (admin) set another user's password.

Without arguments, prompts for old password, then new password twice.
With USER, prompts for new password twice (admin).
With --password-stdin, reads the new password from stdin (no confirmation prompt).
Self mode with --password-stdin still prompts for old password on the TTY.`,
			RunE: runPasswd,
		}
		passwdCmd.Flags().Bool("password-stdin", false, "Read new password from stdin")
		root.AddCommand(passwdCmd)
	})
}

func runPasswd(cmd *cobra.Command, args []string) error {
	stdinPass, _ := cmd.Flags().GetBool("password-stdin")

	if len(args) == 0 {
		return changeOwnPassword(cmd, stdinPass)
	}
	return changeUserPassword(cmd, args[0], stdinPass)
}

func changeOwnPassword(cmd *cobra.Command, stdinPass bool) error {
	oldPass, err := readOldPasswordFn()
	if err != nil {
		return fmt.Errorf("failed to read password: %w", err)
	}

	var newPass []byte
	if stdinPass {
		newPass, err = io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("failed to read new password from stdin: %w", err)
		}
	} else {
		p1, err := readNewPasswordFn()
		if err != nil {
			return fmt.Errorf("failed to read password: %w", err)
		}
		p2, err := readConfirmPasswordFn()
		if err != nil {
			return fmt.Errorf("failed to read password: %w", err)
		}
		if string(p1) != string(p2) {
			return usagef("passwords do not match")
		}
		newPass = p1
	}

	body := map[string]string{
		"oldPassword": strings.TrimRight(string(oldPass), "\n\r"),
		"newPassword": strings.TrimRight(string(newPass), "\n\r"),
	}
	return putPassword(cmd, "/me/password", body)
}

func changeUserPassword(cmd *cobra.Command, username string, stdinPass bool) error {
	if stdinPass {
		newPass, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("failed to read password from stdin: %w", err)
		}
		body := map[string]string{"newPassword": strings.TrimRight(string(newPass), "\n\r")}
		return putPassword(cmd, "/users/"+username+"/password", body)
	}

	p1, err := readNewPasswordFn()
	if err != nil {
		return fmt.Errorf("failed to read password: %w", err)
	}
	p2, err := readConfirmPasswordFn()
	if err != nil {
		return fmt.Errorf("failed to read password: %w", err)
	}
	if string(p1) != string(p2) {
		return usagef("passwords do not match")
	}
	body := map[string]string{"newPassword": strings.TrimRight(string(p1), "\n\r")}
	return putPassword(cmd, "/users/"+username+"/password", body)
}

func putPassword(cmd *cobra.Command, path string, body map[string]string) error {
	httpResp, err := rawRequest(cmd, "PUT", path, toJSON(body))
	if err != nil {
		return err
	}
	defer func() { _ = httpResp.Body.Close() }()
	if httpResp.StatusCode >= 400 {
		b, _ := io.ReadAll(httpResp.Body)
		return errFromResponse(b, httpResp.StatusCode)
	}
	fmt.Println("password updated")
	return nil
}
