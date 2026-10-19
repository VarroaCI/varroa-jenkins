package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/varroaci/varroa-jenkins/internal/bus"
	"github.com/varroaci/varroa-jenkins/internal/recordstore"
)

// runNotePostWait bounds one note post's wait for the recorder's reply.
// The recorder's own database budget is shorter.
const runNotePostWait = 5 * time.Second

// ErrRunNotePostNotDelivered reports that no record store answered the
// note post. The note was not written.
var ErrRunNotePostNotDelivered = errors.New("no record store received the run note")

// RunNotePoster sends one note post to the recorder and answers its reply.
// The recorder owns every write to varroa.run_notes.
type RunNotePoster interface {
	PostRunNote(ctx context.Context, req recordstore.RunNotePostRequest) (recordstore.RunNotePostResponse, error)
}

// busRunNotePoster is the RunNotePoster over the core NATS request-reply
// subject bus.RunNotePostSubject.
type busRunNotePoster struct {
	conn *bus.Conn
}

// NewBusRunNotePoster answers a RunNotePoster over conn.
func NewBusRunNotePoster(conn *bus.Conn) RunNotePoster {
	return &busRunNotePoster{conn: conn}
}

// PostRunNote encodes req, sends it, and decodes the reply. A request with
// no responder answers an error that wraps ErrRunNotePostNotDelivered.
func (b *busRunNotePoster) PostRunNote(ctx context.Context,
	req recordstore.RunNotePostRequest) (recordstore.RunNotePostResponse, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return recordstore.RunNotePostResponse{}, fmt.Errorf("encode run note: %w", err)
	}
	msg, err := b.conn.RequestWithContext(ctx, bus.RunNotePostSubject, data, runNotePostWait)
	if err != nil {
		if bus.IsNoResponder(err) {
			return recordstore.RunNotePostResponse{}, fmt.Errorf("%w: %v", ErrRunNotePostNotDelivered, err)
		}
		return recordstore.RunNotePostResponse{}, fmt.Errorf("request run note post: %w", err)
	}
	var resp recordstore.RunNotePostResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		return recordstore.RunNotePostResponse{}, fmt.Errorf("decode run note reply: %w", err)
	}
	return resp, nil
}
