package model

// SummaryGenerationSpec is the normalized, user-confirmed configuration
// schema. It is distinct from an Agent draft spec, whose inferred values may
// be defaulted. Merely decoding this schema does NOT establish executability:
// source authorization, supported fields, timezone and Workflow projection
// must be validated by the configuration adapter before enabling replay.
type SummaryGenerationSpec struct {
	SchemaVersion int                        `json:"schema_version"`
	Sources       []SummaryGenerationSource  `json:"sources"`
	SummaryMode   int                        `json:"summary_mode"`
	Collaboration string                     `json:"collaboration"` // single or team
	Participants  []string                   `json:"participants"`
	TimeSelector  SummaryTimeSelector        `json:"time_selector"`
	Requirement   *string                    `json:"requirement"` // nil is missing, never task.Title
	Template      *SummaryGenerationTemplate `json:"template"`
	Retrieval     SummaryRetrievalRules      `json:"retrieval"`
	CitationRules SummaryCitationRules       `json:"citation_rules"`
	FieldSources  map[string]string          `json:"field_sources"`
}

type SummaryGenerationSource struct {
	SourceID     string `json:"source_id"`
	SourceType   int    `json:"source_type"`
	Confirmation string `json:"confirmation"`
}

// Relative selectors remain rolling windows; natural_period selectors have
// calendar boundaries. Incremental watermarks are successful applied ranges,
// not schedule.last_run_at. Runtime resolution belongs to the shared executor
// adapter and must produce one frozen absolute range for the whole run.
type SummaryTimeSelector struct {
	Mode         string `json:"mode"` // absolute, relative, natural_period, incremental
	Timezone     string `json:"timezone"`
	Start        string `json:"start,omitempty"`
	End          string `json:"end,omitempty"`
	Days         int    `json:"days,omitempty"`
	Unit         string `json:"unit,omitempty"`
	Offset       int    `json:"offset,omitempty"`
	InitialStart string `json:"initial_start,omitempty"`
}

type SummaryGenerationTemplate struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Content string `json:"content"`
}

type SummaryRetrievalRules struct {
	AuthorIDs []string `json:"author_ids"`
	Keywords  []string `json:"keywords"`
}

type SummaryCitationRules struct {
	Policy string `json:"policy"`
}

// SummaryGenerationSnapshot binds content to what actually executed, not to
// whichever configuration happens to be current at read time.
type SummaryGenerationSnapshot struct {
	Spec             SummaryGenerationSpec `json:"spec"`
	ConfigRevision   int64                 `json:"config_revision"`
	EffectiveAt      string                `json:"effective_at"`
	ResolvedStart    string                `json:"resolved_start"`
	ResolvedEnd      string                `json:"resolved_end"`
	Boundary         string                `json:"boundary"` // explicit retrieval boundary convention
	Executor         string                `json:"executor"`
	Model            string                `json:"model"`
	GenerationID     string                `json:"generation_id"`
	DataGapTruncated bool                  `json:"data_gap_truncated"`
}
