//go:build cgo

package service

import (
	"context"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newDMSourceNameTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE "user" (uid TEXT, name TEXT)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO "user" VALUES ('creator', '创建者'), ('peer-user', 'momo_test'), ('empty-name', '  '), ('outsider', '无关用户')`).Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func TestResolveSourceNameForActor_DM(t *testing.T) {
	db := newDMSourceNameTestDB(t)
	for _, tt := range []struct {
		name, sourceID, actorID, want string
	}{
		{"legacy peer UID", "peer-user", "creator", "momo_test(私聊)"},
		{"creator first", "creator@peer-user", "creator", "momo_test(私聊)"},
		{"creator last", "peer-user@creator", "creator", "momo_test(私聊)"},
		{"other perspective", "creator@peer-user", "peer-user", "创建者(私聊)"},
		{"unrelated actor", "creator@peer-user", "outsider", "来源-creator@(私聊)"},
		{"no actor", "creator@peer-user", "", "来源-creator@(私聊)"},
		{"missing peer", "creator@", "creator", "来源-creator@(私聊)"},
		{"missing first", "@creator", "creator", "来源-@creator(私聊)"},
		{"extra separator", "creator@peer-user@outsider", "creator", "来源-creator@(私聊)"},
		{"self pair", "creator@creator", "creator", "来源-creator@(私聊)"},
		{"unknown user", "creator@unknown", "creator", "来源-creator@(私聊)"},
		{"empty name", "creator@empty-name", "creator", "来源-creator@(私聊)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveSourceNameForActor(tt.sourceID, model.SourceDirect, tt.actorID, db); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
	if got := ResolveSourceNameForActor("creator@peer-user", model.SourceDirect, "creator", nil); got != "来源-creator@(私聊)" {
		t.Fatalf("nil IM DB: %q", got)
	}
	// The actor-less compatibility API must not guess either participant.
	if got := ResolveSourceNameWithType("creator@peer-user", model.SourceDirect, db); got != "来源-creator@(私聊)" {
		t.Fatalf("actor-less canonical ID: %q", got)
	}
}

func TestResolveStoredSourceName_DMSnapshots(t *testing.T) {
	db := newDMSourceNameTestDB(t)
	for _, tt := range []struct {
		name, sourceID, stored, actor, want string
		sourceType                          int
	}{
		{"empty canonical", "creator@peer-user", "", "creator", "momo_test(私聊)", model.SourceDirect},
		{"old placeholder", "creator@peer-user", "来源-creator@(私聊)", "creator", "momo_test(私聊)", model.SourceDirect},
		{"real snapshot", "creator@peer-user", "创建时的名字(私聊)", "creator", "创建时的名字(私聊)", model.SourceDirect},
		{"similar name", "creator@peer-user", "来源-别的名字(私聊)", "creator", "来源-别的名字(私聊)", model.SourceDirect},
		{"nonmember", "creator@peer-user", "来源-creator@(私聊)", "outsider", "来源-creator@(私聊)", model.SourceDirect},
		{"legacy snapshot", "peer-user", "旧私聊名(私聊)", "creator", "旧私聊名(私聊)", model.SourceDirect},
		{"legacy empty", "peer-user", "", "creator", "momo_test(私聊)", model.SourceDirect},
		{"group placeholder", "creator@peer-user", "来源-creator@(群聊)", "creator", "来源-creator@(群聊)", model.SourceGroup},
		{"legacy placeholder", "peer-user", "来源-peer-use(私聊)", "creator", "来源-peer-use(私聊)", model.SourceDirect},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := model.SummarySource{SourceType: tt.sourceType, SourceID: tt.sourceID, SourceName: tt.stored}
			if got := ResolveStoredSourceName(source, tt.actor, db); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
	source := model.SummarySource{SourceType: model.SourceDirect, SourceID: "creator@peer-user", SourceName: "来源-creator@(私聊)"}
	if got := ResolveStoredSourceName(source, "creator", nil); got != source.SourceName {
		t.Fatalf("missing DB must retain deterministic placeholder: %q", got)
	}
}

func TestSummaryWorkflowCanonicalDMSourceName(t *testing.T) {
	for _, target := range []string{"personal", "team"} {
		t.Run(target, func(t *testing.T) {
			svc, db := newSummaryWorkflowTestService(t)
			svc.imDB = newDMSourceNameTestDB(t)
			in := AgentCreateSummaryWorkflowInput{
				ActorID: "creator", SpaceID: "space-1", Requirement: "总结私聊",
				Sources:        []SummaryWorkflowSource{{SourceType: model.SourceDirect, SourceID: "peer-user@creator"}},
				IdempotencyKey: "dm-source-name-" + target,
			}
			var result CreateSummaryWorkflowResult
			var err error
			if target == "team" {
				in.Participants = []SummaryWorkflowParticipant{{UserID: "peer-user"}}
				result, err = svc.CreateTeamFromAgent(context.Background(), in)
			} else {
				result, err = svc.CreatePersonalFromAgent(context.Background(), in)
			}
			if err != nil {
				t.Fatal(err)
			}
			var source model.SummarySource
			if err := db.Where("task_id = ?", result.Task.ID).Take(&source).Error; err != nil {
				t.Fatal(err)
			}
			if source.SourceName != "momo_test(私聊)" || source.SourceID != "peer-user@creator" {
				t.Fatalf("source = %+v", source)
			}
		})
	}
}
