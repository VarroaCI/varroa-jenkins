package controller

import "strings"

// DefaultJenkinsUpstreamURL is the Jenkins upstream host the operator reads
// when VARROA_JENKINS_UPSTREAM_URL is absent.
const DefaultJenkinsUpstreamURL = "https://updates.jenkins.io"

// JenkinsUpstreamState names how the Jenkins upstream metadata host is set.
// The BFF reports it so a surface can explain why no upgrade candidates
// appear.
type JenkinsUpstreamState string

const (
	// JenkinsUpstreamOnline means the built-in upstream host is used.
	JenkinsUpstreamOnline JenkinsUpstreamState = "online"
	// JenkinsUpstreamMirror means an operator-set host is used.
	JenkinsUpstreamMirror JenkinsUpstreamState = "mirror"
	// JenkinsUpstreamOff means the upstream reads are turned off.
	JenkinsUpstreamOff JenkinsUpstreamState = "off"
)

// JenkinsUpstreamURLFromEnv resolves the Jenkins upstream metadata host. An
// absent variable keeps DefaultJenkinsUpstreamURL. An empty variable returns
// ""; the caller then turns off every read from the upstream host. A trailing
// slash is removed so callers can append paths.
func JenkinsUpstreamURLFromEnv(lookup func(string) (string, bool)) string {
	v, ok := lookup("VARROA_JENKINS_UPSTREAM_URL")
	if !ok {
		return DefaultJenkinsUpstreamURL
	}
	return strings.TrimRight(strings.TrimSpace(v), "/")
}

// ClassifyJenkinsUpstream names the state of a resolved upstream URL as
// JenkinsUpstreamURLFromEnv returns it.
func ClassifyJenkinsUpstream(url string) JenkinsUpstreamState {
	switch url {
	case "":
		return JenkinsUpstreamOff
	case DefaultJenkinsUpstreamURL:
		return JenkinsUpstreamOnline
	default:
		return JenkinsUpstreamMirror
	}
}
