package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/varroaci/varroa-jenkins/pkg/client"
)

// runnerRegisterPollInterval and runnerRegisterTimeout govern the poll
// runRunnerRegister runs after create, waiting for the operator to fill
// status.token (inbound) or status.nodeName (ssh). Tests shrink both so the
// timeout path does not take sixty real seconds.
var (
	runnerRegisterPollInterval = time.Second
	runnerRegisterTimeout      = 60 * time.Second
)

func init() {
	registerRootCommand(addRunnerGroup)
}

// addRunnerGroup registers the top-level "runner" command group: register,
// list, and delete. list and delete are thin aliases over the runners noun
// verbs (runners.go); register is the only new behavior (design D12).
func addRunnerGroup(root *cobra.Command) {
	runnerCmd := &cobra.Command{
		Use:   "runner",
		Short: "Manage external runners",
	}

	registerCmd := &cobra.Command{
		Use:   "register NAME --tenant TENANT --remote-fs PATH",
		Short: "Register an external runner and print the launch instruction",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runRunnerRegister,
	}
	registerCmd.Flags().String("tenant", "", "the pipelines Tenant that owns this runner (required)")
	registerCmd.Flags().StringSlice("labels", nil, "unprefixed label suffixes; the operator prefixes each one with <tenant>-")
	registerCmd.Flags().String("launcher", "inbound", "connection direction: inbound or ssh")
	registerCmd.Flags().String("host", "", "the SSH target (ssh launcher only)")
	registerCmd.Flags().Int("port", 0, "the SSH port (ssh launcher only)")
	registerCmd.Flags().String("credential", "", "the Secret name holding the SSH credential (ssh launcher only)")
	registerCmd.Flags().String("remote-fs", "", "the agent work directory on the node (required)")
	registerCmd.Flags().Int("executors", 0, "the node's executor count (default 1)")
	registerCmd.Flags().String("java-path", "", "an explicit java binary path (ssh launcher only)")
	_ = registerCmd.MarkFlagRequired("tenant")
	_ = registerCmd.MarkFlagRequired("remote-fs")
	runnerCmd.AddCommand(registerCmd)

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List runners",
		Args:  cobra.NoArgs,
		RunE:  makeRunGet(runnersNounOpts),
	}
	listCmd.Flags().String("tenant", "", "filter by tenant (tenant)")
	runnerCmd.AddCommand(listCmd)

	deleteCmd := &cobra.Command{
		Use:   "delete NAME",
		Short: "Delete a runner",
		Args:  cobra.ExactArgs(1),
		RunE:  makeRunDelete(runnersNounOpts),
	}
	runnerCmd.AddCommand(deleteCmd)

	root.AddCommand(runnerCmd)
}

// runRunnerRegister implements "varroactl runner register NAME" (design
// D12): validate the name locally, create the Runner, poll it until the
// operator fills the field the launcher needs, then print the launch
// instruction.
func runRunnerRegister(cmd *cobra.Command, args []string) error {
	if len(args) != 1 || args[0] == "" {
		return usagef("runner name is required")
	}
	name := args[0]
	if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
		return usagef("invalid runner name %q: %s", name, strings.Join(errs, "; "))
	}

	tenant, _ := cmd.Flags().GetString("tenant")
	remoteFS, _ := cmd.Flags().GetString("remote-fs")
	launcherFlag, _ := cmd.Flags().GetString("launcher")
	launcher := client.ComponentsSchemasRunnerSpecLauncher(launcherFlag)

	spec := client.ComponentsSchemasRunnerSpec{
		Tenant:   tenant,
		Launcher: launcher,
		RemoteFS: remoteFS,
	}
	if labels, _ := cmd.Flags().GetStringSlice("labels"); len(labels) > 0 {
		spec.Labels = &labels
	}
	if executors, _ := cmd.Flags().GetInt("executors"); executors > 0 {
		spec.NumExecutors = &executors
	}
	if launcher == client.Ssh {
		if host, _ := cmd.Flags().GetString("host"); host != "" {
			spec.Host = &host
		}
		if cmd.Flags().Changed("port") {
			port, _ := cmd.Flags().GetInt("port")
			spec.Port = &port
		}
		if cred, _ := cmd.Flags().GetString("credential"); cred != "" {
			spec.CredentialRef = &cred
		}
		if javaPath, _ := cmd.Flags().GetString("java-path"); javaPath != "" {
			spec.JavaPath = &javaPath
		}
	}

	c, err := apiClient(cmd)
	if err != nil {
		return err
	}

	createResp, err := c.CreateRunnerWithResponse(cmd.Context(), client.ComponentsSchemasRunnerCreateRequest{
		Name: name,
		Spec: spec,
	})
	if err != nil {
		return err
	}
	if createResp.StatusCode() >= 400 {
		return errFromResponse(createResp.Body, createResp.StatusCode())
	}

	entry, timedOut, err := pollRunnerReady(cmd.Context(), c, name, launcher)
	if err != nil {
		return err
	}
	if timedOut {
		return reportRunnerRegisterTimeout(name, entry)
	}

	return printRunnerRegistration(cmd, tenant, name, entry)
}

// pollRunnerReady polls GET /api/v1/runners/{name} until the operator fills
// status.token (inbound) or status.nodeName (ssh), or the deadline passes.
func pollRunnerReady(
	ctx context.Context,
	c *client.ClientWithResponses,
	name string,
	launcher client.ComponentsSchemasRunnerSpecLauncher,
) (*client.ComponentsSchemasRunnerEntry, bool, error) {
	deadline := time.Now().Add(runnerRegisterTimeout)
	for {
		resp, err := c.GetRunnerWithResponse(ctx, name)
		if err != nil {
			return nil, false, err
		}
		if resp.StatusCode() >= 400 {
			return nil, false, errFromResponse(resp.Body, resp.StatusCode())
		}

		entry := resp.JSON200
		if runnerIsReady(entry, launcher) {
			return entry, false, nil
		}
		if time.Now().After(deadline) {
			return entry, true, nil
		}

		select {
		case <-ctx.Done():
			return entry, false, ctx.Err()
		case <-time.After(runnerRegisterPollInterval):
		}
	}
}

// runnerIsReady reports whether the operator has filled the field the
// launcher needs: status.token for inbound, status.nodeName for ssh.
func runnerIsReady(entry *client.ComponentsSchemasRunnerEntry, launcher client.ComponentsSchemasRunnerSpecLauncher) bool {
	if entry == nil {
		return false
	}
	if launcher == client.Inbound {
		return entry.Status.Token != nil && *entry.Status.Token != ""
	}
	return entry.Status.NodeName != nil && *entry.Status.NodeName != ""
}

// reportRunnerRegisterTimeout implements the timeout path: print the
// object and the current Ready reason, and exit with code 1 (a local
// deadline is not a usage fault).
func reportRunnerRegisterTimeout(name string, entry *client.ComponentsSchemasRunnerEntry) error {
	if entry != nil {
		doc, err := runnerEntryToDoc(entry)
		if err == nil {
			_ = printYAML(os.Stdout, doc)
		}
	}
	return fmt.Errorf("timed out waiting for runner %q to become ready (Ready reason: %s)", name, readyReason(entry))
}

// readyReason reads the Ready condition's reason, or "Unknown" when the
// runner carries no such condition yet.
func readyReason(entry *client.ComponentsSchemasRunnerEntry) string {
	if entry == nil || entry.Status.Conditions == nil {
		return "Unknown"
	}
	for _, cond := range *entry.Status.Conditions {
		if cond.Type == "Ready" {
			if cond.Reason != nil && *cond.Reason != "" {
				return *cond.Reason
			}
			return "Unknown"
		}
	}
	return "Unknown"
}

// printRunnerRegistration prints the launch instruction for a ready
// runner: the inbound command for an inbound runner, the SSH expectation
// for an ssh runner.
func printRunnerRegistration(cmd *cobra.Command, tenant, name string, entry *client.ComponentsSchemasRunnerEntry) error {
	if entry.Spec.Launcher == client.Ssh {
		_, _ = fmt.Fprint(os.Stdout, sshInstruction(entry.Spec))
		return nil
	}

	rc, err := resolveContext(func(n string) string {
		f := cmd.Flag(n)
		if f == nil {
			return ""
		}
		return f.Value.String()
	})
	if err != nil {
		return err
	}

	token := ""
	if entry.Status.Token != nil {
		token = *entry.Status.Token
	}
	nodeName := tenant + "-" + name
	if entry.Status.NodeName != nil && *entry.Status.NodeName != "" {
		nodeName = *entry.Status.NodeName
	}
	doorURL := strings.TrimRight(rc.server, "/") + "/runners/" + token + "/"

	_, _ = fmt.Fprintln(os.Stdout, inboundInstruction(doorURL, nodeName, token, entry.Spec.RemoteFS))
	return nil
}

// inboundInstruction renders the exact launch command design D12 states:
//
//	java -jar agent.jar -url https://<door host>/runners/<token>/ \
//	  -name <tenant>-<runner> -secret <token> -webSocket -workDir <remoteFS>
func inboundInstruction(doorURL, nodeName, token, remoteFS string) string {
	return fmt.Sprintf(
		"java -jar agent.jar -url %s \\\n  -name %s -secret %s -webSocket -workDir %s",
		doorURL, nodeName, token, remoteFS,
	)
}

// sshInstruction renders the ssh registration expectation: the host and
// port the cell dials, the Secret name and its three data keys, the label
// the Secret needs, the remote FS directory, and the java path when the
// runner declared one (design D12, D5).
func sshInstruction(spec client.ComponentsSchemasRunnerSpec) string {
	host := ""
	if spec.Host != nil {
		host = *spec.Host
	}
	port := 22
	if spec.Port != nil {
		port = *spec.Port
	}
	cred := ""
	if spec.CredentialRef != nil {
		cred = *spec.CredentialRef
	}

	var b strings.Builder
	fmt.Fprintf(&b, "SSH runner. The cell dials %s:%d.\n", host, port)
	fmt.Fprintf(&b, "Create the Secret %q in the Tenant's agent namespace.\n", cred)
	fmt.Fprintln(&b, "The Secret needs the label varroa.dev/runner-credential=true.")
	fmt.Fprintln(&b, "The Secret needs the data keys username, privateKey, and, optionally, passphrase.")
	fmt.Fprintf(&b, "Remote FS: %s\n", spec.RemoteFS)
	if spec.JavaPath != nil && *spec.JavaPath != "" {
		fmt.Fprintf(&b, "Java path: %s\n", *spec.JavaPath)
	}
	return b.String()
}
