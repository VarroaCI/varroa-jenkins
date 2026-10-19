package sessions

import (
	"context"

	"github.com/google/uuid"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/agent"
	agentstore "github.com/varroaci/varroa-jenkins/internal/agent/store"
	"github.com/varroaci/varroa-jenkins/internal/api/activity"
	"github.com/varroaci/varroa-jenkins/internal/teamagent/instructions"
)

// TurnWriter records the first turn of a session.
type TurnWriter interface {
	SetInstructionsSHA(ctx context.Context, id uuid.UUID, owner, sha string, ev agent.EventInput) error
	AppendEvent(ctx context.Context, id uuid.UUID, g agentstore.Guard, events ...agent.EventInput) ([]agent.Event, error)
}

// Prepare writes the first user event of a session that starts. It reads the
// instructions at the revision that the session pinned at creation, unless the
// data level is none. A resolved read records the revision on the session in
// the same write as the event. Any other outcome writes the event without the
// instructions, so the model sees the trigger and nothing else.
func Prepare(ctx context.Context, w TurnWriter, rd instructions.Reader, cluster string,
	sess agent.Session, ta *v1alpha1.TeamAgent) error {
	revision, _ := sess.Context["revision"].(string)
	repo, _ := sess.Context["repo"].(string)
	message, _ := sess.Context["message"].(string)
	item, _ := sess.Context["item_key"].(string)

	var text string
	path := ""
	resolved := false
	if ta.Spec.Instructions != nil {
		path = ta.Spec.Instructions.Path
		if path == "" {
			path = v1alpha1.DefaultTeamAgentInstructionsPath
		}
	}
	if sess.DataSharing != agent.DataNone && ta.Spec.Instructions != nil {
		// The read uses the repository and revision that the session pinned at
		// creation, not whatever the CatalogSource names now.
		pinned := &v1alpha1.CatalogSource{}
		pinned.Spec.RepoURL = repo
		pinned.Status.ObservedRevision = revision
		res := instructions.Read(ctx, rd, cluster, ta, pinned, revision)
		if res.Reason == v1alpha1.TeamAgentReasonResolved {
			text, resolved = res.Content, true
		}
	}
	ev, err := agent.NewEvent(agent.EventUser, agent.UserPayload{
		Text: TurnText(sess, activity.Event{ItemPath: item, Message: message}, path, revision, text, resolved),
	})
	if err != nil {
		return err
	}
	if resolved {
		// The SHA is the revision that the session pinned, which is the one the
		// read used, not whatever the forge reply reports.
		return w.SetInstructionsSHA(ctx, sess.ID, sess.LeaseOwner, revision, ev)
	}
	_, err = w.AppendEvent(ctx, sess.ID, agentstore.Guard{
		States:     []agent.SessionState{agent.StateRunning},
		LeaseOwner: sess.LeaseOwner,
	}, ev)
	return err
}
