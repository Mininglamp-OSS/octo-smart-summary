package service

import (
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

const ContentContractVersion = 1

// ContentError has a stable machine code; clients must never branch on
// translated human messages.
type ContentError struct {
	Code       string
	HTTPStatus int
}

func (e *ContentError) Error() string { return e.Code }

func contentError(code string, status int) error {
	return &ContentError{Code: code, HTTPStatus: status}
}

type ContentCapabilities struct {
	CanEdit                 bool              `json:"can_edit"`
	CanRefine               bool              `json:"can_refine"`
	CanSaveAsNew            bool              `json:"can_save_as_new"`
	CanConfigureSchedule    bool              `json:"can_configure_schedule"`
	CanSchedule             bool              `json:"can_schedule"`
	CanRegenerateDirect     bool              `json:"can_regenerate_direct"`
	CanRegenerateWithConfig bool              `json:"can_regenerate_with_config"`
	CanViewVersions         bool              `json:"can_view_versions"`
	CanDelete               bool              `json:"can_delete"`
	UnavailableReasons      map[string]string `json:"unavailable_reasons"`
}

type FormalContentVersion struct {
	ContentID              string               `json:"content_id"`
	VersionID              string               `json:"version_id"`
	Version                int                  `json:"version"`
	ContentRevision        int64                `json:"content_revision"`
	Content                string               `json:"content"`
	Citations              []model.Citation     `json:"citations"`
	TeamCitations          []model.TeamCitation `json:"team_citations"`
	CitationVisibility     string               `json:"citation_visibility"`
	TeamCitationVisibility string               `json:"team_citation_visibility"`
	OperationType          string               `json:"operation_type"`
	OperationNote          string               `json:"operation_note"`
	ParentVersionID        string               `json:"parent_version_id,omitempty"`
	BaseContentRevision    int64                `json:"base_content_revision"`
	GenerationID           *string              `json:"generation_id,omitempty"`
	Provisional            bool                 `json:"provisional"`
	IsCurrent              bool                 `json:"is_current"`
	PendingApplication     bool                 `json:"pending_application"`
	EditedAt               *time.Time           `json:"edited_at,omitempty"`
	EditedBy               string               `json:"edited_by,omitempty"`
	RestoredFromVersionID  string               `json:"restored_from_version_id,omitempty"`
	RestoredAt             *time.Time           `json:"restored_at,omitempty"`
	GeneratedAt            time.Time            `json:"generated_at"`
	// Raw snapshots may embed private source identities. Keep these internal;
	// a reader receives only the authorized generation-config status below.
	GenerationSpecSnapshot model.JSON `json:"-"`
}

type FormalContent struct {
	ContentID        string                      `json:"content_id"`
	Kind             string                      `json:"kind"`
	OwnerID          string                      `json:"owner_id,omitempty"`
	IsMain           bool                        `json:"is_main"`
	ContentRevision  int64                       `json:"content_revision"`
	CurrentVersion   *FormalContentVersion       `json:"current_version"`
	Capabilities     ContentCapabilities         `json:"capabilities"`
	GenerationConfig ContentGenerationConfig     `json:"generation_config"`
	ActiveGeneration *model.SummaryGenerationRun `json:"active_generation"`
	Integrity        string                      `json:"integrity"`
}

type ContentGenerationConfig struct {
	State             string   `json:"state"`
	Revision          int64    `json:"revision"`
	MissingFields     []string `json:"missing_fields"`
	UnavailableReason *string  `json:"unavailable_reason"`
}

type FormalContentCatalog struct {
	ContractVersion int             `json:"contract_version"`
	SummaryID       int64           `json:"summary_id"`
	CreatedVia      string          `json:"created_via"`
	MainContentID   string          `json:"main_content_id"`
	Contents        []FormalContent `json:"contents"`
}

type FormalVersionPage struct {
	Items      []FormalContentVersion `json:"items"`
	NextCursor string                 `json:"next_cursor"`
}
