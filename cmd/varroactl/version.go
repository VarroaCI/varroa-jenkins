package main

import (
	"fmt"
	"os"
	"regexp"
	"strconv"

	"github.com/spf13/cobra"
)

var semverRegex = regexp.MustCompile(`^v?(\d+)\.(\d+)(\.\d+)?`)

type parsedVersion struct {
	major int
	minor int
}

func init() {
	registerRootCommand(func(root *cobra.Command) {
		versionCmd := &cobra.Command{
			Use:   "version",
			Short: "Show client and server version information",
			RunE:  runVersion,
		}
		versionCmd.Flags().Bool("client", false, "Only show client version (skip server)")
		root.AddCommand(versionCmd)
	})
}

func runVersion(cmd *cobra.Command, args []string) error {
	clientOnly, _ := cmd.Flags().GetBool("client")

	_, _ = fmt.Fprintf(os.Stdout, "Client Version: %s\n", version)

	if clientOnly {
		return nil
	}

	serverVer, err := resolveServerVersion(cmd)
	if err == nil && serverVer != "" {
		printServerVersion(serverVer)
	} else {
		_, _ = fmt.Fprintln(os.Stdout, "Server Version: unknown")
	}

	return nil
}

func resolveServerVersion(cmd *cobra.Command) (string, error) {
	c, err := apiClient(cmd)
	if err != nil {
		return "", err
	}
	v, err := c.GetVersionWithResponse(cmd.Context())
	if err != nil {
		return "", err
	}
	if v.HTTPResponse.StatusCode >= 400 {
		return "", fmt.Errorf("server error")
	}
	if v.JSON200 == nil {
		return "", fmt.Errorf("unexpected response")
	}
	return v.JSON200.Version, nil
}

func printServerVersion(serverVer string) {
	sv := parseVersion(serverVer)
	cv := parseVersion(version)

	if sv != nil {
		_, _ = fmt.Fprintf(os.Stdout, "Server Version: %s\n", serverVer)
		if cv != nil && (cv.major != sv.major || cv.minor != sv.minor) {
			_, _ = fmt.Fprintf(os.Stderr, "warning: version skew: client %d.%d, server %d.%d\n",
				cv.major, cv.minor, sv.major, sv.minor)
		}
	} else {
		_, _ = fmt.Fprintln(os.Stdout, "Server Version: unknown")
	}
}

func parseVersion(v string) *parsedVersion {
	if v == "dev" {
		return nil
	}
	matches := semverRegex.FindStringSubmatch(v)
	if matches == nil {
		return nil
	}
	major, _ := strconv.Atoi(matches[1])
	minor, _ := strconv.Atoi(matches[2])
	return &parsedVersion{major: major, minor: minor}
}
