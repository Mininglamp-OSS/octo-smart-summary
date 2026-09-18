package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/tokenizer"
	"gorm.io/gorm"
)

const documentSnapshotTruncatedMarker = "\n[文档内容已按长度上限截断]"

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

func loadDocumentEvidence(db *gorm.DB, sources []model.SummarySource, tok tokenizer.Tokenizer, maxTokens int) ([]pipeline.Message, error) {
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
		parts := splitDocumentEvidence(snapshot.Content, tok, maxTokens)
		if snapshot.Truncated && len(parts) > 0 {
			parts[len(parts)-1] += documentSnapshotTruncatedMarker
		}
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

func splitDocumentEvidence(content string, tok tokenizer.Tokenizer, maxTokens int) []string {
	content = strings.TrimSpace(content)
	if content == "" || tok == nil || maxTokens <= 0 {
		return nil
	}
	if tok.Estimate(content) <= maxTokens {
		return []string{content}
	}
	runes := []rune(content)
	parts := make([]string, 0)
	for len(runes) > 0 {
		low, high, end := 1, len(runes), 0
		for low <= high {
			mid := low + (high-low)/2
			if tok.Estimate(string(runes[:mid])) <= maxTokens {
				end = mid
				low = mid + 1
			} else {
				high = mid - 1
			}
		}
		// Always make progress even for a tokenizer whose estimate for one rune
		// exceeds the configured budget. The downstream warning remains the final
		// guard for this pathological case.
		if end == 0 {
			end = 1
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
		version = "｜版本：" + escapeCitationMarkers(message.SourceVersion)
	}
	return fmt.Sprintf("[%d]【文档：%s%s｜片段：%d】\n%s",
		message.CitationIndex, escapeCitationMarkers(message.SourceName), version, message.MessageSeq,
		escapeCitationMarkers(message.Content))
}
