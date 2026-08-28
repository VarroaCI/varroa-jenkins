package controller

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// Jenkins /scriptText returns HTTP 200 even when the script fails to compile or
// throws — the exception text is just part of the response body — so transport
// success says nothing about script success (#529). The operator therefore wraps
// every executeGroovy script in a harness that evaluates it via GroovyShell and
// prints a completion sentinel, and classifies the response by that sentinel.
const (
	groovyOKSentinel     = "__VARROA_GROOVY_OK__"
	groovyFailedSentinel = "__VARROA_GROOVY_FAILED__"
)

// groovyHarness mirrors the script console's execution environment: the same
// binding (so `out`/println behave identically), the plugin uberClassLoader,
// and RemotingDiagnostics' default star-imports. It reprints "Result: <value>"
// for non-null results because /scriptText does that for the outer script and
// existing consumers parse it. The failure sentinel carries the exception's
// first two meaningful message lines — for MultipleCompilationErrorsException
// the first line is just the "startup failed:" header and the actionable
// compiler error is on the second.
const groovyHarness = `def __varroaScript = new String("%s".decodeBase64(), "UTF-8")
def __varroaCfg = new org.codehaus.groovy.control.CompilerConfiguration()
def __varroaImports = new org.codehaus.groovy.control.customizers.ImportCustomizer()
__varroaImports.addStarImports("jenkins", "jenkins.model", "hudson", "hudson.model")
__varroaCfg.addCompilationCustomizers(__varroaImports)
try {
    def __varroaResult = new GroovyShell(jenkins.model.Jenkins.get().pluginManager.uberClassLoader, binding, __varroaCfg).evaluate(__varroaScript)
    if (__varroaResult != null) { println("Result: " + __varroaResult) }
    println("%s")
} catch (Throwable __varroaErr) {
    def __varroaSw = new java.io.StringWriter()
    __varroaErr.printStackTrace(new java.io.PrintWriter(__varroaSw))
    print(__varroaSw.toString())
    def __varroaLines = String.valueOf(__varroaErr).readLines().findAll { it.trim() }
    println("%s " + (__varroaLines ? __varroaLines.take(2).join(" ").trim() : "script threw"))
}
`

// wrapGroovyForClassification embeds the user script (base64, so arbitrary
// content can never break out of the harness) into groovyHarness.
func wrapGroovyForClassification(script string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(script))
	return fmt.Sprintf(groovyHarness, encoded, groovyOKSentinel, groovyFailedSentinel)
}

// classifyGroovyOutput splits a harness response into the script's own output
// and an execution verdict. The harness always prints a sentinel as its final
// line — the FAILED sentinel carries a one-line exception summary after it. A
// response with neither sentinel means the harness itself did not run to
// completion (truncated response, interceptor kill) and is failed.
func classifyGroovyOutput(raw string) (string, error) {
	rest, last := splitLastLine(strings.TrimRight(raw, "\r\n"))
	switch {
	case last == groovyOKSentinel:
		return rest, nil
	case strings.HasPrefix(last, groovyFailedSentinel):
		reason := strings.TrimSpace(strings.TrimPrefix(last, groovyFailedSentinel))
		if reason == "" {
			reason = "script threw"
		}
		return rest, fmt.Errorf("groovy script failed: %s", reason)
	}
	return raw, fmt.Errorf("groovy execution produced no completion marker")
}

func splitLastLine(s string) (rest, line string) {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return "", s
}
