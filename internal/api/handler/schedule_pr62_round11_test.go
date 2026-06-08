package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	mysqldriver "github.com/go-sql-driver/mysql"
)

func TestPR62Round11_OrderedScheduleLockIDs(t *testing.T) {
	old := int64(7)
	first, second := orderedScheduleLockIDs(11, &old)
	if first != 7 {
		t.Fatalf("first=%d want 7", first)
	}
	if second == nil || *second != 11 {
		t.Fatalf("second=%v want 11", second)
	}

	largerOld := int64(42)
	first, second = orderedScheduleLockIDs(11, &largerOld)
	if first != 11 {
		t.Fatalf("first=%d want 11", first)
	}
	if second == nil || *second != 42 {
		t.Fatalf("second=%v want 42", second)
	}

	same := int64(11)
	first, second = orderedScheduleLockIDs(11, &same)
	if first != 11 || second != nil {
		t.Fatalf("same-id lock order = (%d,%v) want (11,nil)", first, second)
	}

	first, second = orderedScheduleLockIDs(11, nil)
	if first != 11 || second != nil {
		t.Fatalf("nil-old lock order = (%d,%v) want (11,nil)", first, second)
	}
}

func TestPR62Round11_IsScheduleRetryableConflict(t *testing.T) {
	for _, err := range []error{
		errRebindConcurrentModified,
		&mysqldriver.MySQLError{Number: 1213, Message: "deadlock found when trying to get lock"},
		&mysqldriver.MySQLError{Number: 1205, Message: "lock wait timeout exceeded"},
		&wrapErr{inner: &mysqldriver.MySQLError{Number: 1213, Message: "deadlock"}},
	} {
		if !isScheduleRetryableConflict(err) {
			t.Fatalf("err=%v should be retryable", err)
		}
	}

	for _, err := range []error{
		nil,
		errors.New("plain"),
		&mysqldriver.MySQLError{Number: 1062, Message: "duplicate"},
	} {
		if isScheduleRetryableConflict(err) {
			t.Fatalf("err=%v should not be retryable", err)
		}
	}
}

func TestPR62Round11_WriteRetryableRebindConflictMapsTo40916(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	writeRetryableRebindConflict(c)

	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d want %d", w.Code, http.StatusConflict)
	}

	var resp apiResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Code != 40916 {
		t.Fatalf("code=%d want 40916", resp.Code)
	}
	if resp.Message != "绑定状态被并发修改，请重试" {
		t.Fatalf("message=%q want retryable conflict message", resp.Message)
	}
}
