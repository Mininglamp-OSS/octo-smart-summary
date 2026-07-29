package handler

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/gin-gonic/gin"
)

func TestCitationsForBotDropsSurroundingContextWithoutMutatingSource(t *testing.T) {
	c, _ := gin.CreateTestContext(nil)
	c.Set("bot_request", true)
	source := []model.Citation{{Index: 1, Content: "quoted", ContextBefore: []model.ContextMsg{{Content: "secret before"}}, ContextAfter: []model.ContextMsg{{Content: "secret after"}}}}
	got := citationsForRequest(c, source)
	if len(got[0].ContextBefore) != 0 || len(got[0].ContextAfter) != 0 {
		t.Fatalf("context was not removed: %+v", got[0])
	}
	if len(source[0].ContextBefore) != 1 || len(source[0].ContextAfter) != 1 {
		t.Fatal("source citations were mutated")
	}
}

func TestCitationsForHumanPreservesSurroundingContext(t *testing.T) {
	c, _ := gin.CreateTestContext(nil)
	source := []model.Citation{{Index: 1, ContextBefore: []model.ContextMsg{{Content: "before"}}}}
	got := citationsForRequest(c, source)
	if len(got[0].ContextBefore) != 1 {
		t.Fatalf("human citation context changed: %+v", got[0])
	}
}
