package main

import "strings"

// defaultJenkinsUpstreamURL is the Jenkins upstream host the operator reads
// when VARROA_JENKINS_UPSTREAM_URL is absent.
const defaultJenkinsUpstreamURL = "https://updates.jenkins.io"

// jenkinsUpstreamURL resolves the Jenkins upstream metadata host. An absent
// variable keeps defaultJenkinsUpstreamURL. An empty variable returns ""; the
// caller then turns off every read from the upstream host. A trailing slash
// is removed so callers can append paths.
func jenkinsUpstreamURL(lookup func(string) (string, bool)) string {
	v, ok := lookup("VARROA_JENKINS_UPSTREAM_URL")
	if !ok {
		return defaultJenkinsUpstreamURL
	}
	return strings.TrimRight(strings.TrimSpace(v), "/")
}
