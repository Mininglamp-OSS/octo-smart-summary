package worker

import "github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"

func channelScopeOptionsForTask(enabled bool, spaceID, agentSessionID string, participantUnion, participantSubset bool) *pipeline.ChannelScopeOptions {
	workspaceTask := pipeline.IsWorkspaceAgentSessionID(agentSessionID)
	if !workspaceTask && !enabled {
		return nil
	}
	opts := &pipeline.ChannelScopeOptions{
		Enabled:                 enabled,
		ParticipantSourceUnion:  workspaceTask && participantUnion,
		ParticipantSourceSubset: workspaceTask && participantSubset,
		WorkspaceTask:           workspaceTask,
	}
	if workspaceTask {
		opts.SpaceID = spaceID
	}
	return opts
}
