package worker

import (
	"fmt"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

// Mixed document+chat worker orchestration (phase 1). See
// docs/mixed-document-chat-summary-development-plan.md §4.4.
//
// A mixed task's persisted summary_source rows carry BOTH chat sources
// (history fetched at execution time via the personal pipeline) and document
// sources (request-time snapshots already persisted by the creation
// transaction). The two evidence classes are loaded separately, share ONE
// citation-index pool, and flow into the same Map/Reduce as pipeline.Message.
// Citation already understands SourceDocument rows (document_id / version /
// chunk), so no schema or citation-format change is needed.

// splitMixedSources partitions persisted source rows into chat sources (for
// the chat fetch pipeline) and document sources (for snapshot loading).
//
// Invariants (plan §4.4 来源分流):
//   - document IDs NEVER enter the chat retrieval input (a document id would
//     either fail channel discovery or, worse, collide with a real channel);
//   - chat sources NEVER enter snapshot loading (a chat source row has no
//     SummarySourceSnapshot row → loadDocumentEvidence would fail);
//   - Derived rows are worker-internal provenance, never user-specified
//     constraints, and are excluded from BOTH sides.
func splitMixedSources(sources []model.SummarySource) (chatSources, documentSources []model.SummarySource) {
	for _, source := range sources {
		if source.Derived {
			continue
		}
		if source.SourceType == model.SourceDocument {
			documentSources = append(documentSources, source)
		} else {
			chatSources = append(chatSources, source)
		}
	}
	return chatSources, documentSources
}

// mixedHasDocumentSource reports whether the persisted sources mix document
// and chat classes. Pure document and pure chat keep their existing
// dedicated paths byte-identical; only a true MIXTURE takes the mixed
// orchestration.
func mixedHasDocumentSource(sources []model.SummarySource) bool {
	hasDoc, hasChat := false, false
	for _, source := range sources {
		if source.Derived {
			continue
		}
		if source.SourceType == model.SourceDocument {
			hasDoc = true
		} else {
			hasChat = true
		}
		if hasDoc && hasChat {
			return true
		}
	}
	return false
}

// appendDocumentEvidence loads document snapshots and appends them to the
// chat messages with CONTINUING citation indexes. The document-side
// CitationIndex values are rewritten here so the two evidence classes share
// one numbering pool across the whole Map/Reduce (plan §4.4 证据编号: 唯一
// 编号贯穿两路,不各自从 1 开始).
//
// documentStartIndex is the first free citation index after the chat side;
// loadDocumentEvidence numbers its chunks from 1, so every document message
// gets offset by (documentStartIndex - 1).
func appendDocumentEvidence(chatMessages []pipeline.Message, documentSources []model.SummarySource, loader documentEvidenceLoader, documentStartIndex int) ([]pipeline.Message, error) {
	if len(documentSources) == 0 {
		return chatMessages, nil
	}
	documentMessages, err := loader(documentSources)
	if err != nil {
		return nil, err
	}
	offset := documentStartIndex - 1
	for i := range documentMessages {
		documentMessages[i].CitationIndex += offset
	}
	return append(chatMessages, documentMessages...), nil
}

// documentEvidenceLoader mirrors loadDocumentEvidence's signature so tests
// can stub snapshot loading without a database.
type documentEvidenceLoader func(sources []model.SummarySource) ([]pipeline.Message, error)

// formatMixedEvidence formats one evidence message for the model prompt:
// document chunks use the document shape (【文档：…】 header with version and
// chunk number), chat messages keep the chat shape ([n][time] sender: …).
// Chat cleaning (bot flags, time trimming) never touches document rows
// because they carry no user sender (plan §4.4 聊天清洗边界).
func formatMixedEvidence(message pipeline.Message) string {
	if message.ChannelType == model.SourceDocument {
		return formatDocumentEvidence(message)
	}
	return fmt.Sprintf("[%d][%s] %s: %s",
		message.CitationIndex, message.SendTime, message.SenderName,
		escapeCitationMarkers(message.Content))
}

// mixedSourceLabel builds the user-prompt source label for a mixed task,
// naming both evidence classes so the model can tell them apart.
func mixedSourceLabel(chatSources, documentSources []model.SummarySource) string {
	label := "混合来源"
	if len(chatSources) > 0 {
		label += fmt.Sprintf("（会话 %d 个", len(chatSources))
	}
	if len(documentSources) > 0 {
		if len(chatSources) > 0 {
			label += "，"
		} else {
			label += "（"
		}
		label += fmt.Sprintf("文档 %d 篇", len(documentSources))
	}
	if len(chatSources) > 0 || len(documentSources) > 0 {
		label += "）"
	}
	return label
}
