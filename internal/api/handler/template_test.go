//go:build cgo

package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupTemplateRouter(t *testing.T) *gin.Engine {
	return setupTemplateRouterWithLimit(t, 0)
}

func setupTemplateRouterWithLimit(t *testing.T, limit int) *gin.Engine {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&model.SummaryUserTemplate{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h := NewTaskHandler(db, nil, "")
	if limit > 0 {
		h.SetCustomTemplateLimit(limit)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.GET("/api/v1/summary-templates", h.GetTemplates)
	r.POST("/api/v1/summary-templates/my", h.CreateCustomTemplate)
	r.PUT("/api/v1/summary-templates/my/:id", h.UpdateCustomTemplate)
	r.DELETE("/api/v1/summary-templates/my/:id", h.DeleteCustomTemplate)
	r.PUT("/api/v1/summary-templates/:id/my", h.UpdateMyTemplate)
	r.DELETE("/api/v1/summary-templates/:id/my", h.DeleteMyTemplate)
	return r
}

func doTemplateReq(r *gin.Engine, method, path string, body interface{}) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	var reader *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", "user1")
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)
	return w
}

func decodeTemplateMap(t *testing.T, w *httptest.ResponseRecorder) map[string]map[string]interface{} {
	t.Helper()
	var resp struct {
		Code int `json:"code"`
		Data struct {
			Templates []map[string]interface{} `json:"templates"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if resp.Code != 0 {
		t.Fatalf("code=%d body=%s", resp.Code, w.Body.String())
	}
	out := map[string]map[string]interface{}{}
	for _, tpl := range resp.Data.Templates {
		out[tpl["id"].(string)] = tpl
	}
	return out
}

func decodeTemplate(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var resp struct {
		Code int `json:"code"`
		Data struct {
			Template map[string]interface{} `json:"template"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	if resp.Code != 0 {
		t.Fatalf("code=%d body=%s", resp.Code, w.Body.String())
	}
	return resp.Data.Template
}

func TestMyTemplateOverrideAndReset(t *testing.T) {
	r := setupTemplateRouter(t)

	customLabel := "项目进展复盘"
	customDescription := "按点分清楚并详细说明风险、负责人和下一步计划"
	w := doTemplateReq(r, http.MethodPut, "/api/v1/summary-templates/project_progress/my", map[string]string{
		"label":       customLabel,
		"description": customDescription,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("put status=%d body=%s", w.Code, w.Body.String())
	}

	w = doTemplateReq(r, http.MethodGet, "/api/v1/summary-templates", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", w.Code, w.Body.String())
	}
	templates := decodeTemplateMap(t, w)
	project := templates["project_progress"]
	if got := project["label"]; got != customLabel {
		t.Fatalf("label=%q want %q", got, customLabel)
	}
	if got := project["description"]; got != customDescription {
		t.Fatalf("description=%q want %q", got, customDescription)
	}
	if got := project["pattern"]; got != customDescription {
		t.Fatalf("pattern=%q want %q", got, customDescription)
	}
	if overridden, _ := project["is_overridden"].(bool); !overridden {
		t.Fatalf("expected is_overridden=true, got %#v", project["is_overridden"])
	}
	if custom, _ := project["is_custom"].(bool); custom {
		t.Fatalf("built-in override should not be is_custom, got %#v", project["is_custom"])
	}

	w = doTemplateReq(r, http.MethodDelete, "/api/v1/summary-templates/project_progress/my", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", w.Code, w.Body.String())
	}
	w = doTemplateReq(r, http.MethodGet, "/api/v1/summary-templates", nil)
	templates = decodeTemplateMap(t, w)
	if got := templates["project_progress"]["label"]; got == customLabel {
		t.Fatalf("label still custom after reset: %q", got)
	}
	if got := templates["project_progress"]["description"]; got == customDescription {
		t.Fatalf("description still custom after reset: %q", got)
	}
	if overridden, _ := templates["project_progress"]["is_overridden"].(bool); overridden {
		t.Fatalf("expected is_overridden=false after reset")
	}
}

func TestMyTemplateDescriptionLimit(t *testing.T) {
	r := setupTemplateRouter(t)

	w := doTemplateReq(r, http.MethodPut, "/api/v1/summary-templates/project_progress/my", map[string]string{
		"label":       "项目进展复盘",
		"description": strings.Repeat("总", 2000),
	})
	if w.Code != http.StatusOK {
		t.Fatalf("2000-rune description status=%d body=%s", w.Code, w.Body.String())
	}

	w = doTemplateReq(r, http.MethodPut, "/api/v1/summary-templates/project_progress/my", map[string]string{
		"label":       "项目进展复盘",
		"description": strings.Repeat("总", 2001),
	})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "总结内容不能超过 2000 字符") {
		t.Fatalf("2001-rune description status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestCustomTemplateCRUD(t *testing.T) {
	r := setupTemplateRouter(t)

	w := doTemplateReq(r, http.MethodPost, "/api/v1/summary-templates/my", map[string]string{
		"label":       "风险复盘",
		"description": "按风险点整理",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("post status=%d body=%s", w.Code, w.Body.String())
	}
	created := decodeTemplate(t, w)
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("custom template id is empty: %#v", created)
	}
	if custom, _ := created["is_custom"].(bool); !custom {
		t.Fatalf("expected is_custom=true, got %#v", created["is_custom"])
	}
	if got := created["pattern"]; got != "按风险点整理" {
		t.Fatalf("created pattern=%q", got)
	}

	w = doTemplateReq(r, http.MethodGet, "/api/v1/summary-templates", nil)
	templates := decodeTemplateMap(t, w)
	if got := templates[id]["label"]; got != "风险复盘" {
		t.Fatalf("custom label=%q", got)
	}

	w = doTemplateReq(r, http.MethodPut, "/api/v1/summary-templates/my/"+id, map[string]string{
		"label":       "风险复盘 v2",
		"description": "更新后的说明",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("put custom status=%d body=%s", w.Code, w.Body.String())
	}
	updated := decodeTemplate(t, w)
	if got := updated["label"]; got != "风险复盘 v2" {
		t.Fatalf("updated label=%q", got)
	}
	if got := updated["pattern"]; got != "更新后的说明" {
		t.Fatalf("updated pattern=%q", got)
	}

	w = doTemplateReq(r, http.MethodDelete, "/api/v1/summary-templates/my/"+id, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("delete custom status=%d body=%s", w.Code, w.Body.String())
	}
	w = doTemplateReq(r, http.MethodGet, "/api/v1/summary-templates", nil)
	templates = decodeTemplateMap(t, w)
	if _, ok := templates[id]; ok {
		t.Fatalf("deleted custom template still returned: %#v", templates[id])
	}
}

func TestCustomTemplateDescriptionLimit(t *testing.T) {
	r := setupTemplateRouter(t)

	w := doTemplateReq(r, http.MethodPost, "/api/v1/summary-templates/my", map[string]string{
		"label":       "长内容模板",
		"description": strings.Repeat("总", 2000),
	})
	if w.Code != http.StatusOK {
		t.Fatalf("2000-rune description status=%d body=%s", w.Code, w.Body.String())
	}

	w = doTemplateReq(r, http.MethodPost, "/api/v1/summary-templates/my", map[string]string{
		"label":       "超长内容模板",
		"description": strings.Repeat("总", 2001),
	})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "总结内容不能超过 2000 字符") {
		t.Fatalf("2001-rune description status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestTemplateLimitResponseDefaultAndConfigured(t *testing.T) {
	r := setupTemplateRouter(t)
	w := doTemplateReq(r, http.MethodGet, "/api/v1/summary-templates", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Code int `json:"code"`
		Data struct {
			CustomTemplateLimit int `json:"custom_template_limit"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Data.CustomTemplateLimit != 30 {
		t.Fatalf("custom_template_limit=%d want 30", resp.Data.CustomTemplateLimit)
	}

	r = setupTemplateRouterWithLimit(t, 2)
	w = doTemplateReq(r, http.MethodGet, "/api/v1/summary-templates", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Data.CustomTemplateLimit != 2 {
		t.Fatalf("custom_template_limit=%d want 2", resp.Data.CustomTemplateLimit)
	}
}

func TestCustomTemplateLimitConfigured(t *testing.T) {
	r := setupTemplateRouterWithLimit(t, 1)

	w := doTemplateReq(r, http.MethodPost, "/api/v1/summary-templates/my", map[string]string{
		"label":       "模板一",
		"description": "总结内容一",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("first post status=%d body=%s", w.Code, w.Body.String())
	}

	w = doTemplateReq(r, http.MethodPost, "/api/v1/summary-templates/my", map[string]string{
		"label":       "模板二",
		"description": "总结内容二",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("second post status=%d want 400 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "自定义模板不能超过 1 个") {
		t.Fatalf("unexpected body=%s", w.Body.String())
	}
}
