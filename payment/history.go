package payment

// CloneAttempt prevents shared history/receipt slices in an in-memory store
// from being mutated before a successful compare-and-swap.
func CloneAttempt(a Attempt) Attempt {
	a.ChannelSnapshot = append([]byte(nil), a.ChannelSnapshot...)
	a.PreviousOrders = append([]string(nil), a.PreviousOrders...)
	a.History = append([]Attempt(nil), a.History...)
	for i := range a.History {
		a.History[i] = CloneAttempt(a.History[i])
	}
	if a.VerifiedFact != nil {
		fact := *a.VerifiedFact
		a.VerifiedFact = &fact
	}
	return a
}

func archived(a Attempt) Attempt {
	a = CloneAttempt(a)
	a.History, a.PreviousOrders, a.SettledProvider = nil, nil, ""
	a.Revision = 0
	return a
}

// FindAttempt searches complete private snapshots, never guesses historical
// credentials from the current channel configuration.
func FindAttempt(root Attempt, channelID, providerOrderID string) (Attempt, bool) {
	if root.ChannelID == channelID && root.ProviderOrderID == providerOrderID {
		return root, true
	}
	for _, a := range root.History {
		if a.ChannelID == channelID && a.ProviderOrderID == providerOrderID {
			return a, true
		}
	}
	return Attempt{}, false
}

func updateAttempt(root *Attempt, a Attempt) bool {
	if root.ChannelID == a.ChannelID && root.ProviderOrderID == a.ProviderOrderID {
		history, previous, revision, settled := root.History, root.PreviousOrders, root.Revision, root.SettledProvider
		*root = a
		root.History, root.PreviousOrders, root.Revision, root.SettledProvider = history, previous, revision, settled
		return true
	}
	for i := range root.History {
		if root.History[i].ChannelID == a.ChannelID && root.History[i].ProviderOrderID == a.ProviderOrderID {
			root.History[i] = archived(a)
			return true
		}
	}
	return false
}
