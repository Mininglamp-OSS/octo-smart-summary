package worker

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

func TestSourceNameForGeneration_ExcludesDerivedRows(t *testing.T) {
	tests := []struct {
		name    string
		sources []model.SummarySource
		want    string
	}{
		{
			name: "derived source is never used as a display label",
			sources: []model.SummarySource{
				{SourceName: "Alice 的私有群", Derived: true},
			},
			want: "多来源",
		},
		{
			name: "one explicit source remains visible when derived rows exist",
			sources: []model.SummarySource{
				{SourceName: "显式群", Derived: false},
				{SourceName: "自动回填群", Derived: true},
			},
			want: "显式群",
		},
		{
			name: "multiple explicit sources use the aggregate label",
			sources: []model.SummarySource{
				{SourceName: "显式群 A", Derived: false},
				{SourceName: "显式群 B", Derived: false},
			},
			want: "多来源",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sourceNameForGeneration(tt.sources); got != tt.want {
				t.Fatalf("sourceNameForGeneration() = %q, want %q", got, tt.want)
			}
		})
	}
}
