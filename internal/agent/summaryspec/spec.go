// Package summaryspec defines the immutable-per-version summary specification
// (SS-03) and the machinery to parse a draft, validate + fill defaults, record
// per-field provenance, and compute a canonical content hash.
//
// The Spec is the single source of truth a run summarizes against. Persisting a
// validated, hashed Spec (rather than re-deriving intent on every tool call) is
// what lets later stages guarantee "all tools read the same Spec" and lets the
// Finish Gate distinguish an explicit user requirement from a model guess.
package summaryspec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// FieldSource is the provenance of a spec field.
type FieldSource string

const (
	SourceUser     FieldSource = "user"     // explicit in the user's request
	SourceUI       FieldSource = "ui"       // provided by the UI (e.g. selected channels)
	SourceDefault  FieldSource = "default"  // filled by validation defaults
	SourceInferred FieldSource = "inferred" // inferred by the model from NL
)

// Allowed enumerations. Unknown values are coerced to the default and the field
// source is downgraded to "default" so a bad model guess can't smuggle an
// out-of-contract value downstream.
var (
	allowedLanguages = map[string]bool{"zh-CN": true, "zh": true, "en": true}
	allowedDetail    = map[string]bool{"brief": true, "standard": true, "detailed": true}
	allowedCitation  = map[string]bool{"every_claim": true, "key_points": true, "none": true}
)

const (
	DefaultLanguage    = "zh-CN"
	DefaultDetailLevel = "standard"
	DefaultCitation    = "every_claim"
)

// TimeRange is an optional [start,end] unix-second window. Zero means unbounded.
type TimeRange struct {
	Start int64 `json:"start,omitempty"`
	End   int64 `json:"end,omitempty"`
}

// Channel is one in-scope channel.
type Channel struct {
	ChannelID string `json:"channel_id"`
	Name      string `json:"name,omitempty"`
	Type      string `json:"type,omitempty"`
}

// Spec is the validated, canonical summary specification.
type Spec struct {
	Objective      string    `json:"objective"`
	Topic          string    `json:"topic"`
	Audience       string    `json:"audience"`
	Language       string    `json:"language"`
	DetailLevel    string    `json:"detail_level"`
	OutputSections []string  `json:"output_sections"`
	Exclusions     []string  `json:"exclusions"`
	CitationPolicy string    `json:"citation_policy"`
	TimeRange      TimeRange `json:"time_range"`
	Channels       []Channel `json:"channels"`
}

// FieldSources maps a spec field name to its provenance.
type FieldSources map[string]FieldSource

// Draft is what a caller (a Planner tool, or the handler from a chat request)
// submits. Pointer fields distinguish "not provided" (nil → default) from
// "explicitly set" (non-nil → user/inferred provenance).
type Draft struct {
	Objective      *string    `json:"objective,omitempty"`
	Topic          *string    `json:"topic,omitempty"`
	Audience       *string    `json:"audience,omitempty"`
	Language       *string    `json:"language,omitempty"`
	DetailLevel    *string    `json:"detail_level,omitempty"`
	OutputSections []string   `json:"output_sections,omitempty"`
	Exclusions     []string   `json:"exclusions,omitempty"`
	CitationPolicy *string    `json:"citation_policy,omitempty"`
	TimeRange      *TimeRange `json:"time_range,omitempty"`
	Channels       []Channel  `json:"channels,omitempty"`
}

// ParseDraft unmarshals a raw JSON draft (e.g. a Planner tool call).
func ParseDraft(raw []byte) (Draft, error) {
	var d Draft
	if err := json.Unmarshal(raw, &d); err != nil {
		return Draft{}, fmt.Errorf("parse spec draft: %w", err)
	}
	return d, nil
}

// Options tune how provenance is assigned for provided fields.
type Options struct {
	// ProvidedSource is the provenance stamped on fields the draft set
	// explicitly. Use SourceInferred when the draft came from model extraction,
	// SourceUser when it came verbatim from the user.
	ProvidedSource FieldSource
	// ChannelSource is the provenance for Channels when the draft provides them
	// (e.g. SourceUI when they came from the UI selection).
	ChannelSource FieldSource
}

// Validate turns a Draft into a canonical Spec, filling defaults and recording
// per-field provenance. It never errors on individual field content — unknown
// enum values are coerced to defaults (with a "default" source) so the pipeline
// always has a valid Spec to run against. It errors only when the draft is
// structurally unusable (nothing to summarize and no scope at all).
func Validate(d Draft, opts Options) (Spec, FieldSources, error) {
	if opts.ProvidedSource == "" {
		opts.ProvidedSource = SourceInferred
	}
	if opts.ChannelSource == "" {
		opts.ChannelSource = opts.ProvidedSource
	}

	src := FieldSources{}
	var s Spec

	// Scalar string fields: provided → trimmed value + provided source;
	// absent/empty → "" + default source.
	s.Objective = setStr(&src, "objective", d.Objective, "", opts.ProvidedSource)
	s.Topic = setStr(&src, "topic", d.Topic, "", opts.ProvidedSource)
	s.Audience = setStr(&src, "audience", d.Audience, "", opts.ProvidedSource)

	// Enum fields: coerce unknown to default and downgrade source to default.
	s.Language = setEnum(&src, "language", d.Language, DefaultLanguage, allowedLanguages, opts.ProvidedSource)
	s.DetailLevel = setEnum(&src, "detail_level", d.DetailLevel, DefaultDetailLevel, allowedDetail, opts.ProvidedSource)
	s.CitationPolicy = setEnum(&src, "citation_policy", d.CitationPolicy, DefaultCitation, allowedCitation, opts.ProvidedSource)

	// List fields.
	s.OutputSections = cleanList(d.OutputSections)
	if len(s.OutputSections) > 0 {
		src["output_sections"] = opts.ProvidedSource
	} else {
		src["output_sections"] = SourceDefault
	}
	s.Exclusions = cleanList(d.Exclusions)
	if len(s.Exclusions) > 0 {
		src["exclusions"] = opts.ProvidedSource
	} else {
		src["exclusions"] = SourceDefault
	}

	// Time range.
	if d.TimeRange != nil {
		s.TimeRange = *d.TimeRange
		src["time_range"] = opts.ProvidedSource
	} else {
		src["time_range"] = SourceDefault
	}

	// Channels.
	s.Channels = cleanChannels(d.Channels)
	if len(s.Channels) > 0 {
		src["channels"] = opts.ChannelSource
	} else {
		src["channels"] = SourceDefault
	}

	// Structural guard: a spec with no objective, no topic, and no channels has
	// nothing to summarize and no scope to resolve — reject rather than persist
	// an empty contract that the Finish Gate would later have to fail anyway.
	if s.Objective == "" && s.Topic == "" && len(s.Channels) == 0 {
		return Spec{}, nil, fmt.Errorf("spec is empty: needs at least an objective, topic, or channel")
	}

	return s, src, nil
}

// Hash returns the sha256 hex of the canonical spec. It marshals a normalized
// copy (sorted lists / channels) so semantically equal specs hash identically
// regardless of input ordering. Provenance is deliberately excluded — the hash
// is about spec content, not how the fields were sourced.
func (s Spec) Hash() (string, error) {
	norm := s
	norm.OutputSections = sortedCopy(s.OutputSections)
	norm.Exclusions = sortedCopy(s.Exclusions)
	norm.Channels = sortedChannels(s.Channels)
	b, err := json.Marshal(norm)
	if err != nil {
		return "", fmt.Errorf("marshal spec for hash: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// JSON returns the spec serialized for the spec_json column.
func (s Spec) JSON() (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("marshal spec: %w", err)
	}
	return string(b), nil
}

// JSON returns field sources serialized for the field_sources column.
func (fs FieldSources) JSON() (string, error) {
	b, err := json.Marshal(fs)
	if err != nil {
		return "", fmt.Errorf("marshal field sources: %w", err)
	}
	return string(b), nil
}

// BuildPromptGuidance renders the spec into a Chinese instruction block injected
// into the Map and Reduce prompts (SS-06), so the model that actually writes the
// local and merged summaries sees the user's requirements — topic focus, target
// audience, output language, detail level, required sections, exclusions, and
// citation policy — instead of a generic, request-blind prompt.
//
// Returns "" for a zero spec (nothing to guide with). The block ends with a
// priority note so these requirements override any default formatting rule in
// the base prompt (e.g. a hardcoded length cap that would contradict
// detail_level=detailed).
func (s Spec) BuildPromptGuidance() string {
	var b strings.Builder
	b.WriteString("\n\n## 本次总结的具体要求（必须严格遵守）\n")
	wrote := false
	line := func(label, val string) {
		if strings.TrimSpace(val) == "" {
			return
		}
		fmt.Fprintf(&b, "- %s：%s\n", label, val)
		wrote = true
	}

	line("总结目标", s.Objective)
	line("主题聚焦", s.Topic)
	line("目标受众", s.Audience)

	if s.Language != "" {
		fmt.Fprintf(&b, "- 输出语言：%s（整篇用该语言输出）\n", s.Language)
		wrote = true
	}
	switch s.DetailLevel {
	case "detailed":
		b.WriteString("- 详细程度：detailed（展开细节，不要压缩为短条目，可保留必要的技术细节）\n")
		wrote = true
	case "brief":
		b.WriteString("- 详细程度：brief（只保留最关键要点，尽量精炼）\n")
		wrote = true
	case "standard", "":
		// standard is the implicit baseline; no extra instruction needed.
	default:
		line("详细程度", s.DetailLevel)
	}
	if len(s.OutputSections) > 0 {
		fmt.Fprintf(&b, "- 必须包含章节：%s（按此结构组织输出）\n", strings.Join(s.OutputSections, "、"))
		wrote = true
	}
	if len(s.Exclusions) > 0 {
		fmt.Fprintf(&b, "- 明确排除：%s（这些内容不要出现在总结中）\n", strings.Join(s.Exclusions, "、"))
		wrote = true
	}
	switch s.CitationPolicy {
	case "every_claim":
		b.WriteString("- 引用要求：每一条结论/要点都必须标注来源引用 [n]\n")
		wrote = true
	case "key_points":
		b.WriteString("- 引用要求：关键结论需标注来源引用 [n]\n")
		wrote = true
	case "none":
		// no citation requirement
	}

	if !wrote {
		return ""
	}
	b.WriteString("\n以上要求优先级高于本提示词中的其它默认格式限制。")
	return b.String()
}

// --- helpers ---

func setStr(src *FieldSources, name string, v *string, def string, provided FieldSource) string {
	if v != nil {
		if t := strings.TrimSpace(*v); t != "" {
			(*src)[name] = provided
			return t
		}
	}
	(*src)[name] = SourceDefault
	return def
}

func setEnum(src *FieldSources, name string, v *string, def string, allowed map[string]bool, provided FieldSource) string {
	if v != nil {
		if t := strings.TrimSpace(*v); t != "" && allowed[t] {
			(*src)[name] = provided
			return t
		}
	}
	(*src)[name] = SourceDefault
	return def
}

func cleanList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func cleanChannels(in []Channel) []Channel {
	out := make([]Channel, 0, len(in))
	seen := map[string]bool{}
	for _, c := range in {
		c.ChannelID = strings.TrimSpace(c.ChannelID)
		if c.ChannelID == "" || seen[c.ChannelID] {
			continue
		}
		seen[c.ChannelID] = true
		out = append(out, c)
	}
	return out
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func sortedChannels(in []Channel) []Channel {
	out := append([]Channel(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i].ChannelID < out[j].ChannelID })
	return out
}
