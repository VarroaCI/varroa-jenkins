package mite

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

	mitev1 "github.com/varroaci/varroa-jenkins/internal/mite/proto/mitev1"
)

// pendingImperativeMsg wraps cmd in the OperatorMessage a pending ack holds.
func pendingImperativeMsg(t *testing.T, cmd *mitev1.ImperativeCommand) *nats.Msg {
	t.Helper()
	data, err := json.Marshal(&mitev1.OperatorMessage{Message: &mitev1.OperatorMessage_Imperative{Imperative: cmd}})
	if err != nil {
		t.Fatalf("marshal pending command: %v", err)
	}
	return &nats.Msg{Data: data}
}

// TestRecordCommandOutcome_RefusedRunCommandsLog asserts a refused cancel,
// input answer, or replay logs one Warn line with and without a record
// store, and acknowledges. A success logs nothing.
func TestRecordCommandOutcome_RefusedRunCommandsLog(t *testing.T) {
	cases := []struct {
		name string
		cmd  *mitev1.ImperativeCommand
		want string
	}{
		{"cancel", &mitev1.ImperativeCommand{CommandId: "cancel-build-r1", Type: mitev1.CommandTypeCancelBuild,
			CancelBuild: &mitev1.CancelBuildPayload{RunId: "r1", JobPath: "acme/app", Actor: "alice"}}, "cancel build command refused"},
		{"proceed", &mitev1.ImperativeCommand{CommandId: "proceed-r1", Type: mitev1.CommandTypeProceedInput,
			ProceedInput: &mitev1.ProceedInputPayload{RunId: "r1", JobPath: "acme/app", InputId: "i"}}, "run input command refused"},
		{"abort", &mitev1.ImperativeCommand{CommandId: "abort-r1", Type: mitev1.CommandTypeAbortInput,
			AbortInput: &mitev1.AbortInputPayload{RunId: "r1", JobPath: "acme/app", InputId: "i"}}, "run input command refused"},
		{"replay", &mitev1.ImperativeCommand{CommandId: "replay-r1", Type: mitev1.CommandTypeReplayBuild,
			ReplayBuild: &mitev1.ReplayBuildPayload{RunId: "r1", JobPath: "acme/app"}}, "replay build command refused"},
	}
	for _, tc := range cases {
		for _, storeEnabled := range []bool{false, true} {
			var logs bytes.Buffer
			h := &BusHandler{recordStoreEnabled: storeEnabled,
				Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))}
			result := &mitev1.CommandResult{
				CommandId: tc.cmd.CommandId, Success: false, Error: "build not found",
				HttpStatus: 404, CommandType: tc.cmd.Type, JobPath: "acme/app", RunId: "r1", Actor: "alice",
			}
			if !h.recordCommandOutcome("cells", "cell-1", pendingImperativeMsg(t, tc.cmd), result) {
				t.Fatalf("%s store=%v: want an ack for a refused command", tc.name, storeEnabled)
			}
			out := logs.String()
			for _, want := range []string{"level=WARN", tc.want, "controller=cells/cell-1", "command_id=" + tc.cmd.CommandId,
				"run_id=r1", "actor=alice", "http_status=404", `error="build not found"`} {
				if !strings.Contains(out, want) {
					t.Fatalf("%s store=%v: log %q does not name %q", tc.name, storeEnabled, out, want)
				}
			}

			logs.Reset()
			result.Success, result.Error = true, ""
			if !h.recordCommandOutcome("cells", "cell-1", pendingImperativeMsg(t, tc.cmd), result) {
				t.Fatalf("%s store=%v: want an ack for a success", tc.name, storeEnabled)
			}
			if logs.Len() != 0 {
				t.Fatalf("%s store=%v: success logged %q, want nothing", tc.name, storeEnabled, logs.String())
			}
		}
	}
}
