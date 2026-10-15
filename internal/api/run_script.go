package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/varroaci/varroa-jenkins/internal/mite"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
	"github.com/varroaci/varroa-jenkins/internal/recordstore/blob"
)

const (
	// runScriptMaxObjectBytes bounds the build.xml a script read opens.
	// One long pipeline's record must not hold the BFF's memory.
	runScriptMaxObjectBytes = 8 << 20

	// runScriptMaxScriptBytes bounds a replayable script and a replay
	// body's script. The command travels over NATS, whose default maximum
	// payload is 1 MiB.
	runScriptMaxScriptBytes = 256 << 10
)

// The reasons a script read names when a run does not replay, in the
// order runScriptSource tests them.
const (
	replayReasonNotTerminal     = "not-terminal"
	replayReasonTemplateDefined = "template-defined"
	replayReasonMaskedParameter = "masked-parameter"
	replayReasonNoReplica       = "no-replica"
	replayReasonUnreadable      = "unreadable"
	replayReasonNotSandbox      = "not-sandbox"
	replayReasonScriptTooLarge  = "script-too-large"
)

// The two definition kinds a script read names.
const (
	definitionKindScript   = "script"
	definitionKindTemplate = "template"
)

// RunScript is GET /runs/{run}/script's response
// (components/schemas.yaml#RunScript). Every field is always present, so
// a reader needs no absent-versus-empty rule. LoadedScripts is never nil.
type RunScript struct {
	Script         string            `json:"script"`
	LoadedScripts  map[string]string `json:"loadedScripts"`
	DefinitionKind string            `json:"definitionKind"`
	Replayable     bool              `json:"replayable"`
	Reason         string            `json:"reason"`
}

// refusedRunScript is the answer for a run that does not replay. It holds
// an empty script and an empty loaded-script map, so a refused run
// reveals no source.
func refusedRunScript(kind, reason string) RunScript {
	return RunScript{LoadedScripts: map[string]string{}, DefinitionKind: kind, Reason: reason}
}

// definitionKind answers "template" when the run's stored payload names a
// template or a marker, and "script" otherwise. It reads the keys through
// runTemplatePayloadOf, which knows the envelope nesting.
func definitionKind(run recordstore.Run) string {
	tp := runTemplatePayloadOf(run.Payload)
	if tp.TemplateName != "" || tp.MarkerPath != "" {
		return definitionKindTemplate
	}
	return definitionKindScript
}

// hasMaskedParameter reports whether a stored parameter value is the
// masked sentinel. Replaying such a run would pass the sentinel as the
// value.
func hasMaskedParameter(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var params map[string]any
	if json.Unmarshal(raw, &params) != nil {
		return false
	}
	for _, v := range params {
		if s, ok := v.(string); ok && s == mite.MaskedParamValue {
			return true
		}
	}
	return false
}

// runScriptSource reads a run's script replica and decides whether the run
// replays. The read path answers its result to the caller. The write path
// refuses a replay whose result is not replayable, so one rule set governs
// the button and the endpoint.
//
// The rules run in a fixed order, and the first failure names itself.
// The first three read the row alone, so a run that cannot replay costs
// no object read. A missing object answers no-replica, never unreadable:
// the record's file list outlives the objects for a capped run and for a
// run that ended before the terminal cleanup kept its build.xml.
//
// It answers an error only for a store failure other than a missing
// object. The caller must hold a non-nil blob store.
func (s *Server) runScriptSource(ctx context.Context, run recordstore.Run) (RunScript, error) {
	kind := definitionKind(run)
	if run.Status != recordstore.StatusCompleted && run.Status != recordstore.StatusDuplicate {
		return refusedRunScript(kind, replayReasonNotTerminal), nil
	}
	if kind == definitionKindTemplate {
		return refusedRunScript(kind, replayReasonTemplateDefined), nil
	}
	if hasMaskedParameter(run.Params) {
		return refusedRunScript(kind, replayReasonMaskedParameter), nil
	}
	key, size, ok := recordstore.RunScriptObject(run)
	if !ok {
		return refusedRunScript(kind, replayReasonNoReplica), nil
	}
	// The recorded size is tested before the object opens. A byte limit
	// on the reader alone would let a truncated document parse.
	if size > runScriptMaxObjectBytes {
		return refusedRunScript(kind, replayReasonUnreadable), nil
	}
	// The gateway stores every run-state object through Replace, and the
	// bucket store writes a Replace object as part 0 under the key. Only
	// the range reader assembles those parts; the object reader opens the
	// bare key and finds nothing there.
	body, err := s.deps.BlobStore.NewRangeReader(ctx, key, 0, -1)
	if err != nil {
		if errors.Is(err, blob.ErrObjectNotFound) {
			return refusedRunScript(kind, replayReasonNoReplica), nil
		}
		return RunScript{}, fmt.Errorf("open run script %s: %w", key, err)
	}
	defer func() { _ = body.Close() }()

	script, loaded, sandbox, err := parseRunScript(io.LimitReader(body, runScriptMaxObjectBytes))
	if err != nil {
		return refusedRunScript(kind, replayReasonUnreadable), nil
	}
	if !sandbox {
		return refusedRunScript(kind, replayReasonNotSandbox), nil
	}
	if len(script) > runScriptMaxScriptBytes {
		return refusedRunScript(kind, replayReasonScriptTooLarge), nil
	}
	return RunScript{Script: script, LoadedScripts: loaded, DefinitionKind: kind, Replayable: true}, nil
}

var (
	errRunScriptNoExecution = errors.New("build record holds no execution element")
	errRunScriptNoScript    = errors.New("execution element holds no script")
)

// parseRunScript reads the script, the loaded scripts, and the sandbox
// flag out of one build.xml.
//
// It scans tokens to the first "execution" start element and then reads
// that element's direct children alone. A struct decode would bind an
// element named "script" anywhere in the document, and a build.xml holds
// many actions this package does not know.
//
// The loaded-script map is read as XStream writes a Map: a sequence of
// "entry" elements, each holding a "string" key and a "string" value. An
// entry of another shape is skipped, and the script still answers. The
// loaded map is never nil.
func parseRunScript(r io.Reader) (script string, loaded map[string]string, sandbox bool, err error) {
	dec := xml.NewDecoder(skipXMLDeclaration(r))
	if err := seekStartElement(dec, "execution"); err != nil {
		return "", nil, false, err
	}
	loaded = map[string]string{}
	haveScript := false
	for {
		tok, tokErr := dec.Token()
		if tokErr != nil {
			return "", nil, false, fmt.Errorf("read execution element: %w", tokErr)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "script":
				text, textErr := readElementText(dec)
				if textErr != nil {
					return "", nil, false, textErr
				}
				script, haveScript = text, true
			case "loadedScripts":
				if loadErr := readLoadedScripts(dec, loaded); loadErr != nil {
					return "", nil, false, loadErr
				}
			case "sandbox":
				text, textErr := readElementText(dec)
				if textErr != nil {
					return "", nil, false, textErr
				}
				sandbox = strings.TrimSpace(text) == "true"
			default:
				if skipErr := dec.Skip(); skipErr != nil {
					return "", nil, false, fmt.Errorf("skip execution child %s: %w", t.Name.Local, skipErr)
				}
			}
		case xml.EndElement:
			if !haveScript {
				return "", nil, false, errRunScriptNoScript
			}
			return script, loaded, sandbox, nil
		}
	}
}

// skipXMLDeclaration drops a leading XML declaration. Jenkins writes
// build.xml as XML 1.1, and encoding/xml refuses every version but 1.0.
// The body holds no construct that differs between the two versions.
func skipXMLDeclaration(r io.Reader) io.Reader {
	br := bufio.NewReader(r)
	head, _ := br.Peek(512)
	trimmed := bytes.TrimPrefix(head, []byte("\xef\xbb\xbf"))
	if !bytes.HasPrefix(trimmed, []byte("<?xml")) {
		return br
	}
	end := bytes.Index(head, []byte("?>"))
	if end < 0 {
		return br
	}
	_, _ = br.Discard(end + len("?>"))
	return br
}

// seekStartElement reads tokens up to and including the first start
// element named local.
func seekStartElement(dec *xml.Decoder, local string) error {
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return errRunScriptNoExecution
		}
		if err != nil {
			return fmt.Errorf("scan build record: %w", err)
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == local {
			return nil
		}
	}
}

// readElementText answers the character data of the element whose start
// the decoder just read, and consumes its end element. A nested element
// is skipped.
func readElementText(dec *xml.Decoder) (string, error) {
	var sb strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", fmt.Errorf("read element text: %w", err)
		}
		switch t := tok.(type) {
		case xml.CharData:
			sb.Write(t)
		case xml.StartElement:
			if err := dec.Skip(); err != nil {
				return "", fmt.Errorf("skip nested element: %w", err)
			}
		case xml.EndElement:
			return sb.String(), nil
		}
	}
}

// readLoadedScripts reads the entries of the loadedScripts element whose
// start the decoder just read into loaded, and consumes its end element.
func readLoadedScripts(dec *xml.Decoder, loaded map[string]string) error {
	for {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("read loaded scripts: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local != "entry" {
				if err := dec.Skip(); err != nil {
					return fmt.Errorf("skip loaded-script child: %w", err)
				}
				continue
			}
			key, value, ok, entryErr := readLoadedScriptEntry(dec)
			if entryErr != nil {
				return entryErr
			}
			if ok {
				loaded[key] = value
			}
		case xml.EndElement:
			return nil
		}
	}
}

// readLoadedScriptEntry reads one entry element whose start the decoder
// just read. It answers ok only for an entry that holds exactly two
// "string" children and nothing else.
func readLoadedScriptEntry(dec *xml.Decoder) (key, value string, ok bool, err error) {
	var strs []string
	wellFormed := true
	for {
		tok, tokErr := dec.Token()
		if tokErr != nil {
			return "", "", false, fmt.Errorf("read loaded-script entry: %w", tokErr)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local != "string" {
				wellFormed = false
				if skipErr := dec.Skip(); skipErr != nil {
					return "", "", false, fmt.Errorf("skip loaded-script entry child: %w", skipErr)
				}
				continue
			}
			text, textErr := readElementText(dec)
			if textErr != nil {
				return "", "", false, textErr
			}
			strs = append(strs, text)
		case xml.EndElement:
			if !wellFormed || len(strs) != 2 {
				return "", "", false, nil
			}
			return strs[0], strs[1], true, nil
		}
	}
}
