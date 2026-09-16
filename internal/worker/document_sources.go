package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"gorm.io/gorm"
)

const documentEvidenceChunkRunes = 40000

func documentSourcesOnly(sources []model.SummarySource) bool {
	if len(sources) == 0 {
		return false
	}
	for _, source := range sources {
		if source.SourceType != model.SourceDocument {
			return false
		}
	}
	return true
}

func hasDocumentSource(sources []model.SummarySource) bool {
	for _, source := range sources {
		if source.SourceType == model.SourceDocument {
			return true
		}
	}
	return false
}

func loadDocumentEvidence(db *gorm.DB, sources []model.SummarySource) ([]pipeline.Message, error) {
	messages := make([]pipeline.Message, 0, len(sources))
	for _, source := range sources {
		var snapshot model.SummarySourceSnapshot
		if err := db.Where("summary_source_id = ?", source.ID).First(&snapshot).Error; err != nil {
			return nil, fmt.Errorf("load document snapshot source=%d: %w", source.ID, err)
		}
		hash := sha256.Sum256([]byte(snapshot.Content))
		actualHash := hex.EncodeToString(hash[:])
		if actualHash != source.SourceHash || actualHash != snapshot.ContentHash {
			return nil, fmt.Errorf("document snapshot hash mismatch source=%d", source.ID)
		}
		parts := splitDocumentEvidence(snapshot.Content, documentEvidenceChunkRunes)
		for index, content := range parts {
			messages = append(messages, pipeline.Message{
				MessageSeq:    int64(index + 1),
				SenderUID:     source.SourceID,
				SenderName:    source.SourceName,
				ChannelID:     source.SourceID,
				ChannelType:   model.SourceDocument,
				Content:       content,
				SourceName:    source.SourceName,
				SourceVersion: source.SourceVersion,
			})
		}
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("document snapshots contain no text")
	}
	return messages, nil
}

func splitDocumentEvidence(content string, maxRunes int) []string {
	content = strings.TrimSpace(content)
	if content == "" || maxRunes <= 0 {
		return nil
	}
	if utf8.RuneCountInString(content) <= maxRunes {
		return []string{content}
	}
	runes := []rune(content)
	parts := make([]string, 0, (len(runes)+maxRunes-1)/maxRunes)
	for len(runes) > 0 {
		end := maxRunes
		if end > len(runes) {
			end = len(runes)
		}
		if end < len(runes) {
			for i := end; i > end/2; i-- {
				if runes[i-1] == '\n' {
					end = i
					break
				}
			}
		}
		part := strings.TrimSpace(string(runes[:end]))
		if part != "" {
			parts = append(parts, part)
		}
		runes = runes[end:]
	}
	return parts
}

func formatDocumentEvidence(message pipeline.Message) string {
	version := ""
	if message.SourceVersion != "" {
		version = "｜版本：" + message.SourceVersion
	}
	return fmt.Sprintf("[%d]【文档：%s%s｜片段：%d】\n%s",
		message.CitationIndex, message.SourceName, version, message.MessageSeq,
		escapeCitationMarkers(message.Content))
}
