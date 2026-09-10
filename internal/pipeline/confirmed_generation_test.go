//go:build cgo

package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

func TestConfirmedGenerationSourcesBoundariesAndFailures(t *testing.T) {
	db := setupPipelineImDB(t)
	ctx := context.Background()
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	seedPipelineThreads(db, start.Unix())
	db.Exec("INSERT INTO space_member VALUES ('space1','user1',1)")
	input := service.FrozenGenerationInput{
		Spec: model.SummaryGenerationSpec{Sources: []model.SummaryGenerationSource{{SourceID: "grp1____thA", SourceType: model.SourceThread}}},
		Time: service.ResolvedGenerationTime{Start: start, End: start.Add(time.Second), Boundary: "[start,end)"},
	}
	fetch := func() ([]Message, error) {
		return FetchConfirmedGenerationMessages(ctx, "space1", "user1", input, db, nil, "mysql", 1, 100, 1, 1)
	}
	messages, err := fetch()
	if err != nil || len(messages) != 1 {
		t.Fatalf("start boundary not included: %d %v", len(messages), err)
	}
	input.Time.Start = start.Add(-time.Second)
	input.Time.End = start
	messages, err = fetch()
	if err != nil || len(messages) != 0 {
		t.Fatalf("end boundary not excluded: %d %v", len(messages), err)
	}
	input.Spec.Sources[0].SourceType = model.SourceGroup
	if _, err = fetch(); err == nil {
		t.Fatal("source type mismatch authorized")
	}
	input.Spec.Sources[0].SourceType = model.SourceThread
	db.Exec("UPDATE space_member SET status=0")
	if _, err = fetch(); err == nil {
		t.Fatal("inactive space membership authorized")
	}
	db.Exec("UPDATE space_member SET status=1")
	input.Time.Start = start
	input.Time.End = start.Add(time.Minute)
	input.Spec.Retrieval.Keywords = []string{"does not match"}
	messages, err = fetch()
	if err != nil || len(messages) != 0 {
		t.Fatal("confirmed keyword filter ignored")
	}
	input.Spec.Retrieval.Keywords = nil
	db.Exec("ALTER TABLE message RENAME TO missing_message_fixture")
	if _, err = fetch(); err == nil {
		t.Fatal("failed retrieval silently produced an empty successful summary")
	}
}

func TestConfirmedMySQLCoverageMustBeComplete(t *testing.T) {
	db := setupPipelineImDB(t)
	seedPipelineThreads(db, 100)
	db.Exec(`INSERT INTO message VALUES (9,'user1','grp1____thA',5,101,?,0)`, []byte(`{"type":1,"content":"another message"}`))
	_, err := fetchViaMySQL(context.Background(), []ChannelInfo{{ChannelID: "grp1____thA", ChannelType: 5}}, "user1", 90, 110, db, 1, 1, 1, true)
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("truncated confirmed scope was accepted: %v", err)
	}
}
