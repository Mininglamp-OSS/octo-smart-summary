package handler

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

func TestSummarySearchWireRanges(t *testing.T) {
	for _, tc := range []struct {
		name, title, topic, query, want string
	}{
		{"title only", "😀ＯＣＴＯ", "其他", "octo", `{"title_ranges":[[1,5]],"topic_ranges":[]}`},
		{"topic only", "标题", "😀方案评审", "方案", `{"title_ranges":[],"topic_ranges":[[1,3]]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(matchSummarySearch(model.SummaryTask{Title: tc.title, Topic: tc.topic}, normalizedSummaryKeyword(tc.query)))
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != tc.want {
				t.Fatalf("wire ranges = %s, want %s", encoded, tc.want)
			}
		})
	}
}

func TestSummarySearchLiteralMatching(t *testing.T) {
	for _, tc := range []struct {
		name, title, topic, query string
		titleRanges, topicRanges  [][2]int
	}{
		{"title", "周会排期", "方案评审", " 排期 ", [][2]int{{2, 4}}, nil},
		{"topic", "周会排期", "方案评审", "方案评审", nil, [][2]int{{0, 4}}},
		{"both", "方案排期", "方案评审", "方案", [][2]int{{0, 2}}, [][2]int{{0, 2}}},
		{"width and case", "😀ＯｃＴｏ方案", "other", "octo", [][2]int{{1, 5}}, nil},
		{"query width", "Octo", "other", "ＯＣＴＯ", [][2]int{{0, 4}}, nil},
		{"all positions", "排期、排期", "other", "排期", [][2]int{{0, 2}, {3, 5}}, nil},
		{"overlapping", "ababa", "other", "aba", [][2]int{{0, 5}}, nil},
		{"case expansion", "Straße", "other", "STRASSE", [][2]int{{0, 6}}, nil},
		{"punctuation", "Octo方案v1.2.pdf", "other", "v1.2", [][2]int{{6, 10}}, nil},
		{"punctuation kept", "v1.2", "other", "v12", nil, nil},
		{"internal space kept", "排期与评审", "other", "排期 评审", nil, nil},
		{"literal percent", "100%完成", "other", "%", [][2]int{{3, 4}}, nil},
		{"literal underscore", "a_b", "other", "_", [][2]int{{1, 2}}, nil},
		{"literal slash", `a\b`, "other", `\`, [][2]int{{1, 2}}, nil},
		{"no wildcard", "abc", "other", "%", nil, nil},
		{"no pinyin", "排期", "other", "paiqi", nil, nil},
		{"no traditional conversion", "方案评审", "other", "方案評審", nil, nil},
		{"separate fields", "周会", "排期", "周会排期", nil, nil},
		{"blank", "title", "topic", " \t\n ", nil, nil},
		{"display topic fallback", "title", " \t", "title", [][2]int{{0, 5}}, [][2]int{{0, 5}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := model.SummaryTask{Title: tc.title, Topic: tc.topic}
			match := matchSummarySearch(task, normalizedSummaryKeyword(tc.query))
			wantTitle, wantTopic := tc.titleRanges, tc.topicRanges
			if wantTitle == nil {
				wantTitle = [][2]int{}
			}
			if wantTopic == nil {
				wantTopic = [][2]int{}
			}
			if !reflect.DeepEqual(match.TitleRanges, wantTitle) || !reflect.DeepEqual(match.TopicRanges, wantTopic) {
				t.Fatalf("match = %+v, want title=%v topic=%v", match, wantTitle, wantTopic)
			}
		})
	}
}
