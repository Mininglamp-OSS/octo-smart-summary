package handler

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

func TestDocumentFrontedObjectSeparatorRegression(t *testing.T) {
	for _, object := range []string{"表格", "行动项", "附件内容", "图表", "表格和行动项"} {
		for _, separator := range []string{"、", " ", "　"} {
			message := object + separator + "不要总结"
			if !hasNegatedSummaryRunIntent(message) {
				t.Errorf("separator erased before negative action: %q", message)
			}
			for _, preview := range []bool{false, true} {
				if got := classifySummaryWorkspaceIntent(message, preview); got != service.SummaryIntentExplain {
					t.Errorf("%q preview=%t: got=%s", message, preview, got)
				}
			}
		}
	}
	// Preserve established content constraints; no new grammar is introduced.
	for _, message := range []string{
		"表格不用生成", "请总结，表格不用生成",
		"总结这份文档 表格不用生成", "请总结，不要生成表格和行动项",
	} {
		if hasNegatedSummaryRunIntent(message) {
			t.Errorf("existing content constraint regressed: %q", message)
		}
	}
}

func TestDocumentContentScanPreservesLongLists(t *testing.T) {
	for _, separator := range []string{"和", "与", "以及", "、", " and "} {
		object := strings.Repeat("表格"+separator, 128) + "行动项"
		if !isSummaryWorkspaceContentObject(object) {
			t.Errorf("valid list rejected solely for length: separator=%q", separator)
		}
		if isSummaryWorkspaceContentObject(object + separator + "总结") {
			t.Errorf("unknown member accepted: separator=%q", separator)
		}
	}
	for _, object := range []string{"", "表格和", "表格和未知对象", strings.Repeat("不生成", 2730)} {
		if isSummaryWorkspaceContentObject(object) {
			t.Errorf("invalid content object accepted: %.40q", object)
		}
	}
}

func TestDocumentRepeatedVerbRegression(t *testing.T) {
	for _, phrase := range []string{"不生成", "不要生成", "don't generate "} {
		message := strings.Repeat(phrase, maxMessageLen/utf8.RuneCountInString(phrase))
		if !hasNegatedSummaryRunIntent(message) {
			t.Errorf("max-length negative input executed: %q", phrase)
		}
	}
}

func BenchmarkDocumentRepeatedVerbs(b *testing.B) {
	for _, phrase := range []string{"不生成", "不要生成", "don't generate "} {
		for _, size := range []int{1024, 4096, maxMessageLen} {
			message := strings.Repeat(phrase, size/utf8.RuneCountInString(phrase))
			b.Run(fmt.Sprintf("%s/%d", phrase, size), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					hasNegatedSummaryRunIntent(message)
				}
			})
		}
	}
}
