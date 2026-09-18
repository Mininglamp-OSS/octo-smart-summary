package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

const (
	documentSummaryFetchTimeout      = 30 * time.Second
	maxDocumentSummaryAggregateRunes = 200000
)

type documentSummaryCreateError struct {
	status     int
	code       int
	message    string
	retryAfter string
}

func createSummaryHasDocumentSource(req createSummaryReq) bool {
	for _, source := range req.Sources {
		if source.SourceType == model.SourceDocument {
			return true
		}
	}
	return false
}

func (h *TaskHandler) prepareDocumentSummarySources(
	requestContext context.Context,
	header http.Header,
	spaceID, userID string,
	req createSummaryReq,
) ([]service.SummaryWorkflowSource, bool, *documentSummaryCreateError) {
	documentMode := createSummaryHasDocumentSource(req)
	if !documentMode {
		return nil, false, nil
	}

	badRequest := func(message string) ([]service.SummaryWorkflowSource, bool, *documentSummaryCreateError) {
		return nil, true, &documentSummaryCreateError{status: http.StatusBadRequest, code: 40001, message: message}
	}
	if len(req.Sources) > service.MaxDocumentSummarySourceCount {
		return badRequest("文档来源不能超过10个")
	}
	for _, source := range req.Sources {
		if source.SourceType != model.SourceDocument {
			return badRequest("文档总结不能混合聊天来源")
		}
	}
	if req.UID != "" && req.UID != userID {
		return badRequest("文档总结仅支持当前用户创建")
	}
	if len(req.Participants) != 0 {
		return badRequest("文档总结暂不支持其他参与者")
	}
	if req.TimeRange != nil {
		return badRequest("文档总结不支持时间范围")
	}
	if req.OriginChannelID != "" || req.OriginChannelType != 0 {
		return badRequest("文档总结不支持来源会话")
	}

	refs := make([]documentRefReq, 0, len(req.Sources))
	for _, source := range req.Sources {
		refs = append(refs, documentRefReq{DocumentID: strings.TrimSpace(source.SourceID)})
	}
	sources, err := prepareDocumentSummarySourcesFromRefs(requestContext, h.documentClient, header, spaceID, userID, refs)
	return sources, true, err
}

func prepareDocumentSummarySourcesFromRefs(
	requestContext context.Context,
	client documentSourceClient,
	header http.Header,
	spaceID, userID string,
	refs []documentRefReq,
) ([]service.SummaryWorkflowSource, *documentSummaryCreateError) {
	badRequest := func(message string) ([]service.SummaryWorkflowSource, *documentSummaryCreateError) {
		return nil, &documentSummaryCreateError{status: http.StatusBadRequest, code: 40001, message: message}
	}
	if len(refs) > service.MaxDocumentSummarySourceCount {
		return badRequest("文档来源不能超过10个")
	}
	refs = normalizeDocumentRefs(refs)
	if len(refs) == 0 {
		return badRequest("document_id is required")
	}
	if err := validateDocumentRefs(refs); err != nil {
		return badRequest(err.Error())
	}
	if client == nil {
		return nil, &documentSummaryCreateError{status: http.StatusBadGateway, code: 50201, message: "document summary source API is not configured"}
	}

	fetchContext, cancel := context.WithTimeout(requestContext, documentSummaryFetchTimeout)
	defer cancel()
	type fetchResult struct {
		document *documentSummarySource
		err      error
	}
	results := make([]fetchResult, len(refs))
	var wg sync.WaitGroup
	for i, ref := range refs {
		wg.Add(1)
		go func(index int, documentRef documentRefReq) {
			defer wg.Done()
			results[index].document, results[index].err = client.FetchSummarySource(
				fetchContext, spaceID, userID, documentRef.DocumentID, "", header,
			)
		}(i, ref)
	}
	wg.Wait()

	sources := make([]service.SummaryWorkflowSource, 0, len(refs))
	aggregateRunes := 0
	for i, result := range results {
		if result.err != nil {
			return nil, mapDocumentSummaryCreateError(result.err)
		}
		document := result.document
		if document == nil {
			return nil, &documentSummaryCreateError{status: http.StatusBadGateway, code: 50202, message: "文档服务暂不可用"}
		}
		normalizeFetchedDocumentSource(document, refs[i])
		content := documentSnapshotContent(document)
		if content == "" {
			return nil, &documentSummaryCreateError{status: http.StatusBadRequest, code: 40004, message: "文档没有可总结内容"}
		}
		aggregateRunes += utf8.RuneCountInString(content)
		if aggregateRunes > maxDocumentSummaryAggregateRunes {
			return nil, &documentSummaryCreateError{status: http.StatusRequestEntityTooLarge, code: 40007, message: "文档总内容过大，请减少文档数量后重试"}
		}
		hash := sha256.Sum256([]byte(content))
		title := document.Title
		if title == "" {
			title = refs[i].DocumentID
		}
		sources = append(sources, service.SummaryWorkflowSource{
			SourceType:        model.SourceDocument,
			SourceID:          refs[i].DocumentID,
			SourceName:        title,
			SourceVersion:     document.Version,
			SourceHash:        hex.EncodeToString(hash[:]),
			SnapshotContent:   content,
			SnapshotTruncated: document.Truncated,
		})
	}
	return sources, nil
}

// documentSnapshotContent follows the same source contract as preview:
// non-empty chunks take precedence over content, and section titles remain part
// of the model-visible text. normalizeFetchedDocumentSource must run first.
func documentSnapshotContent(document *documentSummarySource) string {
	if len(document.Chunks) == 0 {
		return strings.TrimSpace(document.Content)
	}
	var parts []string
	for _, chunk := range document.Chunks {
		text := strings.TrimSpace(chunk.Text)
		if text == "" {
			continue
		}
		if title := strings.TrimSpace(chunk.Title); title != "" {
			text = documentChunkTitlePrefix + title + "\n" + text
		}
		parts = append(parts, text)
	}
	content := strings.Join(parts, "\n\n")
	if utf8.RuneCountInString(content) > maxDocumentPromptRunes {
		document.Truncated = true
		content = truncateRunes(content, maxDocumentPromptRunes)
	}
	return content
}

func mapDocumentSummaryCreateError(err error) *documentSummaryCreateError {
	var sourceError *documentSourceError
	if !errors.As(err, &sourceError) {
		return &documentSummaryCreateError{status: http.StatusBadGateway, code: 50202, message: "文档服务暂不可用"}
	}
	switch {
	case sourceError.status == http.StatusTooManyRequests:
		return &documentSummaryCreateError{status: sourceError.status, code: 42901, message: "文档服务繁忙，请稍后重试", retryAfter: sourceError.retryAfter}
	case sourceError.status >= http.StatusInternalServerError:
		return &documentSummaryCreateError{status: sourceError.status, code: 50202, message: "文档服务暂不可用"}
	default:
		return &documentSummaryCreateError{status: sourceError.status, code: 40003, message: "文档不可访问或尚未解析完成"}
	}
}
