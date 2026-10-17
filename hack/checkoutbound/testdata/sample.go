package sample

import _ "ghcr.io/acme/tool:1.0"

// Comment-only host: https://comment.host.io/path must never be extracted.
var endpoint = "https://api.real-host.io/v1"

var agent = "jenkins/inbound-agent:3384.v60d89463d9e0-1-jdk21"
