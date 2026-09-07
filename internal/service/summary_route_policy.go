package service

// SummaryIntent is the natural-language intent already classified by the
// Agent. The policy combines it with trusted structured context; it does not
// let the model decide whether a side-effecting workflow is allowed.
type SummaryIntent string

const (
	SummaryIntentGenerate SummaryIntent = "generate"
	SummaryIntentRevise   SummaryIntent = "revise"
	SummaryIntentExplain  SummaryIntent = "explain"
	SummaryIntentUnknown  SummaryIntent = "unknown"
)

// SummaryAction is the trusted UI action attached to a request. Chat asks the
// policy to derive a route. A trusted direct-team action bypasses natural-
// language intent classification, while proposal confirmation must satisfy
// persisted-state guards before a side effect is allowed.
type SummaryAction string

const (
	SummaryActionChat              SummaryAction = "chat"
	SummaryActionStartTeamWorkflow SummaryAction = "start_team_workflow"
	SummaryActionConfirmWorkflow   SummaryAction = "confirm_workflow"
)

// SummaryRoute is the next operation allowed by the deterministic policy.
type SummaryRoute string

const (
	SummaryRouteClarification    SummaryRoute = "clarification"
	SummaryRoutePersonalWorkflow SummaryRoute = "personal_workflow"
	SummaryRouteTeamConfirmation SummaryRoute = "team_workflow_confirmation"
	SummaryRouteTeamWorkflow     SummaryRoute = "team_workflow"
	SummaryRouteAgentPreview     SummaryRoute = "agent_preview"
	SummaryRouteAgentRevision    SummaryRoute = "agent_revision"
	SummaryRouteExplanation      SummaryRoute = "explanation"
)

// SummaryRouteInput contains only facts needed to select an execution path.
// Permission checks happen before setting HasValidSource/ParticipantsValid.
type SummaryRouteInput struct {
	Action                     SummaryAction
	Intent                     SummaryIntent
	HasExplicitRunIntent       bool
	HasSelectedSource          bool
	HasValidSource             bool
	HasSelectedTemplate        bool
	HasRequirement             bool
	HasOtherParticipants       bool
	ParticipantsValid          bool
	HasCurrentPreview          bool
	PreviewScopeMatches        bool
	HasTeamProposal            bool
	TeamProposalScopeMatches   bool
	HasEnoughContextForPreview bool
	HasHardMissingData         bool
}

// DeriveSummaryRoute applies the server-side routing boundary for the unified
// entry. Team impact has priority over personal execution. A trusted direct
// team action bypasses proposal confirmation after the server revalidates the
// participant, source, permission, and requirement structure; confirmation
// keeps the persisted proposal guards.
func DeriveSummaryRoute(in SummaryRouteInput) SummaryRoute {
	switch in.Action {
	case SummaryActionConfirmWorkflow:
		if in.HasHardMissingData || !in.HasOtherParticipants || !in.ParticipantsValid ||
			(in.HasSelectedSource && !in.HasValidSource) ||
			(!in.HasSelectedTemplate && !in.HasRequirement) ||
			!in.HasTeamProposal || !in.TeamProposalScopeMatches {
			return SummaryRouteClarification
		}
		return SummaryRouteTeamWorkflow
	case SummaryActionStartTeamWorkflow:
		if in.HasHardMissingData || !in.HasOtherParticipants || !in.ParticipantsValid ||
			(in.HasSelectedSource && !in.HasValidSource) ||
			(!in.HasSelectedTemplate && !in.HasRequirement) {
			return SummaryRouteClarification
		}
		return SummaryRouteTeamWorkflow
	case "", SummaryActionChat:
		// Continue below. Empty keeps zero-value callers backward compatible.
	default:
		return SummaryRouteClarification
	}

	if in.HasHardMissingData || (in.HasSelectedSource && !in.HasValidSource) ||
		(in.HasOtherParticipants && !in.ParticipantsValid) {
		return SummaryRouteClarification
	}
	// Explanation is read-only, but it can still invoke content-reading tools.
	// Never let it bypass source, participant, or reference validation.
	if in.Intent == SummaryIntentExplain {
		return SummaryRouteExplanation
	}

	switch in.Intent {
	case SummaryIntentRevise:
		// Adding collaborators changes the execution scope. An older personal
		// preview must not be revised or saved as if that scope were unchanged;
		// the Agent needs to establish a new team proposal first.
		if in.HasOtherParticipants {
			return SummaryRouteClarification
		}
		if in.HasCurrentPreview && in.PreviewScopeMatches {
			return SummaryRouteAgentRevision
		}
		return SummaryRouteClarification
	case SummaryIntentGenerate:
		if in.HasOtherParticipants {
			// A team workflow can collect source material from its invited
			// participants, so an explicitly selected chat is optional. The
			// user must still provide an actual requirement, either directly
			// or through a selected template. Any selected source has already
			// passed the validity guard above.
			if !in.HasSelectedTemplate && !in.HasRequirement {
				return SummaryRouteClarification
			}
			return SummaryRouteTeamConfirmation
		}
		if in.HasExplicitRunIntent && in.HasSelectedSource && in.HasValidSource && (in.HasSelectedTemplate || in.HasRequirement) {
			return SummaryRoutePersonalWorkflow
		}
		if in.HasEnoughContextForPreview {
			return SummaryRouteAgentPreview
		}
	}

	return SummaryRouteClarification
}
