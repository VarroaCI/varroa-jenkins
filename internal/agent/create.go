package agent

// CreateInput is the state that decides whether a new session starts.
type CreateInput struct {
	Paused            bool
	DayTotal          int64
	DayLimit          int64
	DataLevel         DataSharing
	ProviderAvailable bool
}

// DecideCreate answers the skip reason for a new session. It checks the
// kill switch, the day budget, the data level, and the provider, in that
// order. It returns start true and an empty skip when the session starts.
// It reads no store.
func DecideCreate(in CreateInput) (skip SkipReason, start bool) {
	switch {
	case in.Paused:
		return SkipKillSwitch, false
	case in.DayLimit <= 0 || in.DayTotal >= in.DayLimit:
		return SkipBudgetDay, false
	case !in.DataLevel.Valid() || in.DataLevel == DataNone:
		return SkipDataSharingNone, false
	case !in.ProviderAvailable:
		return SkipProviderUnavailable, false
	}
	return "", true
}
