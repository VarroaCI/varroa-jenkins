package triggers

import (
	"context"
	"testing"

	"github.com/varroaci/varroa-jenkins/internal/agent"
)

// TestCreateDecisionParity proves the create path stores the skip reason that
// agent.DecideCreate gives for the same inputs. Each case uses its own item key,
// so the debounce check does not hide the decision.
func TestCreateDecisionParity(t *testing.T) {
	c := testCreator(t, false)
	cases := []struct {
		name     string
		paused   bool
		provider string
		dataSh   string
		// level is the data level the create path computes for the case.
		level agent.DataSharing
		avail bool
	}{
		{"kill switch", true, "", "", agent.DataLogs, true},
		{"provider unavailable", false, "nope", "", agent.DataLogs, false},
		{"data sharing none", false, "", "none", agent.DataNone, true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c.Settings = func(context.Context) (agent.Settings, error) {
				return agent.Settings{Paused: tc.paused}, nil
			}
			m := testFor("parity-"+string(rune('a'+i)), nil)
			m.ItemKey = "obj/parity-" + string(rune('a'+i))
			m.Event.ItemPath = m.ItemKey
			if tc.provider != "" {
				m.TA.Spec.Provider = tc.provider
			}
			if tc.dataSh != "" {
				m.TA.Spec.DataSharing = tc.dataSh
			}
			sess, out, err := c.Create(context.Background(), m)
			if err != nil || out != Skipped {
				t.Fatalf("create = %v, %v; want skipped", out, err)
			}
			want, _ := agent.DecideCreate(agent.CreateInput{
				Paused:            tc.paused,
				DayLimit:          c.Cfg.Assistant.Budgets.TeamDay,
				DataLevel:         tc.level,
				ProviderAvailable: tc.avail,
			})
			if sess.SkipReason != want {
				t.Fatalf("stored skip %q, DecideCreate gives %q", sess.SkipReason, want)
			}
		})
	}
}
