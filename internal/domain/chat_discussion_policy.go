package domain

// ChatDiscussionPolicy limits new discussion chains in one exact topic.
// Revision zero is the default. Each invitation reserves a wake for the return.
// This policy neither grants permissions nor admits work.
type ChatDiscussionPolicy struct {
	ThreadRef
	Revision         int64 `json:"revision"`
	MaxInviteHops    int   `json:"max_invite_hops"`
	MaxTargetedWakes int   `json:"max_targeted_wakes"`
}
