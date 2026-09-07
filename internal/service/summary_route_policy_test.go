package service

import "testing"

func TestDeriveSummaryRoute(t *testing.T) {
	tests := []struct {
		name string
		in   SummaryRouteInput
		want SummaryRoute
	}{
		{
			name: "explicit team start bypasses confirmation",
			in: SummaryRouteInput{
				Action:               SummaryActionStartTeamWorkflow,
				Intent:               SummaryIntentGenerate,
				HasExplicitRunIntent: true,
				HasRequirement:       true,
				HasOtherParticipants: true,
				ParticipantsValid:    true,
			},
			want: SummaryRouteTeamWorkflow,
		},
		{
			name: "explicit team start trusts the action over free form intent",
			in: SummaryRouteInput{
				Action:               SummaryActionStartTeamWorkflow,
				Intent:               SummaryIntentExplain,
				HasRequirement:       true,
				HasOtherParticipants: true,
				ParticipantsValid:    true,
			},
			want: SummaryRouteTeamWorkflow,
		},
		{
			name: "explicit team start still validates requirements",
			in: SummaryRouteInput{
				Action:               SummaryActionStartTeamWorkflow,
				HasOtherParticipants: true,
				ParticipantsValid:    true,
			},
			want: SummaryRouteClarification,
		},
		{
			name: "explicit team start still validates participants",
			in: SummaryRouteInput{
				Action:               SummaryActionStartTeamWorkflow,
				HasRequirement:       true,
				HasOtherParticipants: true,
			},
			want: SummaryRouteClarification,
		},
		{
			name: "personal workflow requires source template and generate intent",
			in: SummaryRouteInput{
				Intent:               SummaryIntentGenerate,
				HasExplicitRunIntent: true,
				HasSelectedSource:    true,
				HasValidSource:       true,
				HasSelectedTemplate:  true,
			},
			want: SummaryRoutePersonalWorkflow,
		},
		{
			name: "personal workflow accepts a typed requirement without a template",
			in: SummaryRouteInput{
				Intent:               SummaryIntentGenerate,
				HasExplicitRunIntent: true,
				HasSelectedSource:    true,
				HasValidSource:       true,
				HasRequirement:       true,
			},
			want: SummaryRoutePersonalWorkflow,
		},
		{
			name: "inferred source never creates a personal workflow directly",
			in: SummaryRouteInput{
				Intent:                     SummaryIntentGenerate,
				HasExplicitRunIntent:       true,
				HasValidSource:             true,
				HasSelectedTemplate:        true,
				HasEnoughContextForPreview: true,
			},
			want: SummaryRouteAgentPreview,
		},
		{
			name: "template context without explicit run intent stays in agent preview",
			in: SummaryRouteInput{
				Intent:                     SummaryIntentGenerate,
				HasSelectedSource:          true,
				HasValidSource:             true,
				HasSelectedTemplate:        true,
				HasEnoughContextForPreview: true,
			},
			want: SummaryRouteAgentPreview,
		},
		{
			name: "participants and source without requirement cannot start",
			in: SummaryRouteInput{
				Intent:               SummaryIntentGenerate,
				HasSelectedSource:    true,
				HasValidSource:       true,
				HasOtherParticipants: true,
				ParticipantsValid:    true,
			},
			want: SummaryRouteClarification,
		},
		{
			name: "participants alone without requirement cannot start",
			in: SummaryRouteInput{
				Intent:               SummaryIntentGenerate,
				HasOtherParticipants: true,
				ParticipantsValid:    true,
			},
			want: SummaryRouteClarification,
		},
		{
			name: "participants and user requirement start source free team workflow",
			in: SummaryRouteInput{
				Intent:               SummaryIntentGenerate,
				HasRequirement:       true,
				HasOtherParticipants: true,
				ParticipantsValid:    true,
			},
			want: SummaryRouteTeamConfirmation,
		},
		{
			name: "participants and template start source free team workflow",
			in: SummaryRouteInput{
				Intent:               SummaryIntentGenerate,
				HasSelectedTemplate:  true,
				HasOtherParticipants: true,
				ParticipantsValid:    true,
			},
			want: SummaryRouteTeamConfirmation,
		},
		{
			name: "selected source participants and requirement require confirmation",
			in: SummaryRouteInput{
				Intent:               SummaryIntentGenerate,
				HasSelectedSource:    true,
				HasValidSource:       true,
				HasRequirement:       true,
				HasOtherParticipants: true,
				ParticipantsValid:    true,
			},
			want: SummaryRouteTeamConfirmation,
		},
		{
			name: "selected source participants and template require confirmation",
			in: SummaryRouteInput{
				Intent:               SummaryIntentGenerate,
				HasSelectedSource:    true,
				HasValidSource:       true,
				HasSelectedTemplate:  true,
				HasOtherParticipants: true,
				ParticipantsValid:    true,
			},
			want: SummaryRouteTeamConfirmation,
		},
		{
			name: "an invalid selected source blocks source free team fallback",
			in: SummaryRouteInput{
				Intent:               SummaryIntentGenerate,
				HasSelectedSource:    true,
				HasRequirement:       true,
				HasOtherParticipants: true,
				ParticipantsValid:    true,
			},
			want: SummaryRouteClarification,
		},
		{
			name: "invalid participant blocks side effects",
			in: SummaryRouteInput{
				Intent:               SummaryIntentGenerate,
				HasValidSource:       true,
				HasOtherParticipants: true,
			},
			want: SummaryRouteClarification,
		},
		{
			name: "free form request generates preview",
			in: SummaryRouteInput{
				Intent:                     SummaryIntentGenerate,
				HasEnoughContextForPreview: true,
				ParticipantsValid:          true,
			},
			want: SummaryRouteAgentPreview,
		},
		{
			name: "template only can preview after recent source discovery",
			in: SummaryRouteInput{
				Intent:                     SummaryIntentGenerate,
				HasSelectedTemplate:        true,
				HasEnoughContextForPreview: true,
			},
			want: SummaryRouteAgentPreview,
		},
		{
			name: "revision requires an existing preview",
			in: SummaryRouteInput{
				Intent:              SummaryIntentRevise,
				HasCurrentPreview:   true,
				PreviewScopeMatches: true,
			},
			want: SummaryRouteAgentRevision,
		},
		{
			name: "revision without preview clarifies",
			in: SummaryRouteInput{
				Intent: SummaryIntentRevise,
			},
			want: SummaryRouteClarification,
		},
		{
			name: "participant scope invalidates personal preview revision",
			in: SummaryRouteInput{
				Intent:               SummaryIntentRevise,
				HasCurrentPreview:    true,
				PreviewScopeMatches:  true,
				HasOtherParticipants: true,
				ParticipantsValid:    true,
			},
			want: SummaryRouteClarification,
		},
		{
			name: "explanation cannot bypass invalid execution scope",
			in: SummaryRouteInput{
				Intent:               SummaryIntentExplain,
				HasValidSource:       true,
				HasSelectedTemplate:  true,
				HasOtherParticipants: true,
				HasHardMissingData:   true,
			},
			want: SummaryRouteClarification,
		},
		{
			name: "explanation remains available with valid execution scope",
			in: SummaryRouteInput{
				Intent:            SummaryIntentExplain,
				HasSelectedSource: true,
				HasValidSource:    true,
			},
			want: SummaryRouteExplanation,
		},
		{
			name: "confirmed current team proposal creates workflow",
			in: SummaryRouteInput{
				Action:                   SummaryActionConfirmWorkflow,
				HasRequirement:           true,
				HasOtherParticipants:     true,
				ParticipantsValid:        true,
				HasTeamProposal:          true,
				TeamProposalScopeMatches: true,
			},
			want: SummaryRouteTeamWorkflow,
		},
		{
			name: "stale team proposal cannot create workflow",
			in: SummaryRouteInput{
				Action:               SummaryActionConfirmWorkflow,
				HasSelectedSource:    true,
				HasValidSource:       true,
				HasRequirement:       true,
				HasOtherParticipants: true,
				ParticipantsValid:    true,
				HasTeamProposal:      true,
			},
			want: SummaryRouteClarification,
		},
		{
			name: "hard missing data overrides all routes",
			in: SummaryRouteInput{
				Intent:                     SummaryIntentGenerate,
				HasExplicitRunIntent:       true,
				HasSelectedSource:          true,
				HasValidSource:             true,
				HasSelectedTemplate:        true,
				ParticipantsValid:          true,
				HasEnoughContextForPreview: true,
				HasHardMissingData:         true,
			},
			want: SummaryRouteClarification,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DeriveSummaryRoute(tt.in); got != tt.want {
				t.Fatalf("DeriveSummaryRoute() = %q, want %q", got, tt.want)
			}
		})
	}
}
