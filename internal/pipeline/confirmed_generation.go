package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"gorm.io/gorm"
)

type GenerationSourceAuthorizer struct{ DB *gorm.DB }

func confirmedSourceMaps(sources []model.SummaryGenerationSource) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(sources))
	for _, source := range sources {
		out = append(out, map[string]interface{}{"source_id": source.SourceID, "source_type": source.SourceType})
	}
	return out
}

func (a GenerationSourceAuthorizer) AuthorizeGenerationSources(ctx context.Context, space, actor string, sources []model.SummaryGenerationSource) error {
	_, err := a.channels(ctx, space, actor, sources)
	return err
}

func (a GenerationSourceAuthorizer) channels(ctx context.Context, space, actor string, sources []model.SummaryGenerationSource) ([]ChannelInfo, error) {
	if a.DB == nil || space == "" || actor == "" || len(sources) == 0 {
		return nil, &service.ContentError{Code: "generation_sources_unavailable", HTTPStatus: 422}
	}
	var memberships []struct {
		SpaceID string
		UID     string
	}
	if err := a.DB.WithContext(ctx).Table("space_member").Select("space_id, uid").
		Where("space_id = ? AND uid = ? AND status = 1", space, actor).Find(&memberships).Error; err != nil {
		return nil, err
	}
	active := false
	for _, member := range memberships {
		if member.SpaceID == space && member.UID == actor {
			active = true
		}
	}
	if !active {
		return nil, &service.ContentError{Code: "generation_sources_forbidden", HTTPStatus: 403}
	}
	maps := confirmedSourceMaps(sources)
	channels, err := GetUserChannels(ctx, actor, a.DB, WithSpaceID(space), WithSelectedThreads(selectedThreadChannelIDs(maps)))
	if err != nil {
		return nil, err
	}
	// Source type is part of identity. The legacy helper only compares IDs.
	available := map[string]ChannelInfo{}
	for _, channel := range channels {
		available[fmt.Sprintf("%d:%s", channel.ChannelType, channel.ChannelID)] = channel
	}
	out := make([]ChannelInfo, 0, len(sources))
	for _, source := range sources {
		channelType := mapFrontendSourceType(source.SourceType)
		id := NormalizeDMChannelID(source.SourceID, actor, channelType)
		channel, ok := available[fmt.Sprintf("%d:%s", channelType, id)]
		if !ok {
			return nil, &service.ContentError{Code: "generation_sources_forbidden", HTTPStatus: 403}
		}
		out = append(out, channel)
	}
	return out, nil
}

// FetchConfirmedGenerationMessages uses existing permission/discovery and
// backend adapters, but never invokes intent extraction or post-retrieval LLM
// narrowing. The confirmed specification, not a model guess, owns the scope.
func FetchConfirmedGenerationMessages(ctx context.Context, space, actor string, input service.FrozenGenerationInput,
	imDB *gorm.DB, octoClient octoSearchClient, backend string, tableCount, maxPerChannel, concurrency, pollSec int) ([]Message, error) {
	channels, err := (GenerationSourceAuthorizer{DB: imDB}).channels(ctx, space, actor, input.Spec.Sources)
	if err != nil {
		return nil, err
	}
	// Backend timestamps are integer seconds with an inclusive end. Translate
	// [start,end) once; round start up and end down without changing the snapshot.
	start := input.Time.Start.Unix()
	if input.Time.Start.Nanosecond() > 0 {
		start++
	}
	end := input.Time.End.Unix()
	if input.Time.End.Nanosecond() == 0 {
		end--
	}
	if start > end {
		return []Message{}, nil
	}
	var messages []Message
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case "mysql":
		messages, err = fetchViaMySQL(ctx, channels, actor, start, end, imDB, tableCount, maxPerChannel, concurrency, true)
	case "", "batch":
		if octoClient == nil {
			return nil, fmt.Errorf("octo-search client is not configured")
		}
		messages, err = fetchViaBatch(ctx, octoClient, channels, actor, start, end, concurrency, time.Duration(pollSec)*time.Second, true)
	default:
		return nil, fmt.Errorf("unsupported message fetch backend")
	}
	if err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(messages))
	authors := map[string]bool{}
	for _, uid := range input.Spec.Retrieval.AuthorIDs {
		authors[uid] = true
	}
	for _, message := range messages {
		when, err := time.Parse(time.RFC3339, message.SendTime)
		if err != nil || when.Before(input.Time.Start) || !when.Before(input.Time.End) {
			continue
		}
		if len(authors) > 0 && !authors[message.SenderUID] {
			continue
		}
		matches := len(input.Spec.Retrieval.Keywords) == 0
		for _, keyword := range input.Spec.Retrieval.Keywords {
			if strings.Contains(strings.ToLower(message.Content), strings.ToLower(keyword)) {
				matches = true
				break
			}
		}
		if matches {
			out = append(out, message)
		}
	}
	return out, nil
}
