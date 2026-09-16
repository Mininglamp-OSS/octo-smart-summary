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

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

const (
	documentSummaryFetchTimeout = 30 * time.Second
)

type documentSummaryCreateError struct {
	status     int
	code       int
	message    string
	retryAfter string
}

func (h *TaskHandler) prepareDocumentSummarySources(
	requestContext context.Context,
	header http.Header,
	spaceID, userID string,
	req createSummaryReq,
) ([]service.SummaryWorkflowSource, bool, *documentSummaryCreateError) {
	documentMode := false
	for _, source := range req.Sources {
		if source.SourceType == model.SourceDocument {
			documentMode = true
			break
		}
	}
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
	refs = normalizeDocumentRefs(refs)
	if len(refs) == 0 {
		return badRequest("document_id is required")
	}
	if err := validateDocumentRefs(refs); err != nil {
		return badRequest(err.Error())
	}
	if h.documentClient == nil {
		return nil, true, &documentSummaryCreateError{status: http.StatusBadGateway, code: 50201, message: "document summary source API is not configured"}
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
			results[index].document, results[index].err = h.documentClient.FetchSummarySource(
				fetchContext, spaceID, userID, documentRef.DocumentID, "", header,
			)
		}(i, ref)
	}
	wg.Wait()

	sources := make([]service.SummaryWorkflowSource, 0, len(refs))
	for i, result := range results {
		if result.err != nil {
			return nil, true, mapDocumentSummaryCreateError(result.err)
		}
		document := result.document
		if document == nil {
			return nil, true, &documentSummaryCreateError{status: http.StatusBadGateway, code: 50202, message: "文档服务暂不可用"}
		}
		content := documentSnapshotContent(document)
		if content == "" {
			return nil, true, &documentSummaryCreateError{status: http.StatusBadRequest, code: 40004, message: "文档没有可总结内容"}
		}
		hash := sha256.Sum256([]byte(content))
		title := truncateRunes(strings.TrimSpace(document.Title), maxDocumentTitleRunes)
		if title == "" {
			title = refs[i].DocumentID
		}
		version := truncateRunes(strings.TrimSpace(stripForbiddenRefRunes(document.Version)), maxDocumentVersionLen)
		sources = append(sources, service.SummaryWorkflowSource{
			SourceType:      model.SourceDocument,
			SourceID:        refs[i].DocumentID,
			SourceName:      title,
			SourceVersion:   version,
			SourceHash:      hex.EncodeToString(hash[:]),
			SnapshotContent: content,
		})
	}
	return sources, true, nil
}

func documentSnapshotContent(document *documentSummarySource) string {
	if content := strings.TrimSpace(document.Content); content != "" {
		return content
	}
	chunks := make([]string, 0, len(document.Chunks))
	for _, chunk := range document.Chunks {
		if text := strings.TrimSpace(chunk.Text); text != "" {
			chunks = append(chunks, text)
		}
	}
	return strings.Join(chunks, "\n\n")
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
