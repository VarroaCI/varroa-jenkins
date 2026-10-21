// Package guide holds the operator guide that every agent surface reads.
// docs/agents/guide.md is the source. guide.txt is its byte copy, and a
// test fails when the two differ.
package guide

import _ "embed"

// URI is the MCP resource URI of the guide.
const URI = "varroa://guide"

// MaxBytes caps the guide. The system prompt carries it on every provider
// call, and each call counts toward the session token budget.
const MaxBytes = 8192

// Text is the guide as Markdown.
//
//go:embed guide.txt
var Text string
