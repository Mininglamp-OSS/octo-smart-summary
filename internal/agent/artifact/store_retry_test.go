package artifact

import (
	"errors"
	"fmt"
	"testing"

	mysqldriver "github.com/go-sql-driver/mysql"
)

func TestIsRetryableFreezeTransactionError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "mysql deadlock number",
			err:  &mysqldriver.MySQLError{Number: 1213, Message: "Deadlock found when trying to get lock"},
			want: true,
		},
		{
			name: "mysql lock wait timeout number",
			err:  &mysqldriver.MySQLError{Number: 1205, Message: "Lock wait timeout exceeded"},
			want: true,
		},
		{
			name: "wrapped mysql deadlock",
			err:  fmt.Errorf("freeze transaction: %w", &mysqldriver.MySQLError{Number: 1213, Message: "Deadlock found"}),
			want: true,
		},
		{
			name: "gorm mysql error string",
			err:  errors.New("Error 1213 (40001): Deadlock found when trying to get lock; try restarting transaction"),
			want: true,
		},
		{
			name: "lock wait string",
			err:  errors.New("Error 1205 (HY000): Lock wait timeout exceeded; try restarting transaction"),
			want: true,
		},
		{
			name: "not every numeric neighbor retries",
			err:  errors.New("Error 12050: unrelated application code"),
			want: false,
		},
		{
			name: "non-lock error",
			err:  errors.New("duplicate key value violates unique constraint"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryableFreezeTransactionError(tt.err); got != tt.want {
				t.Fatalf("isRetryableFreezeTransactionError() = %v, want %v", got, tt.want)
			}
		})
	}
}
