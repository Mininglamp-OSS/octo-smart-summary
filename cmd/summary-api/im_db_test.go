package main

import (
	"errors"
	"testing"

	"gorm.io/gorm"
)

func TestRequiredIMDBStartup(t *testing.T) {
	unavailable := errors.New("connection refused")
	connected := &gorm.DB{}
	for _, tc := range []struct {
		name, dsn string
		handle    *gorm.DB
		err       error
		wantOpen  bool
		wantError bool
	}{
		{name: "missing DSN", wantError: true},
		{name: "blank DSN", dsn: " \t", wantError: true},
		{name: "startup outage", dsn: "configured", err: unavailable, wantOpen: true, wantError: true},
		{name: "nil handle", dsn: "configured", wantOpen: true, wantError: true},
		{name: "restart after recovery", dsn: "configured", handle: connected, wantOpen: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			got, err := openRequiredIMDB(tc.dsn, func(dsn string) (*gorm.DB, error) {
				called = true
				if dsn != tc.dsn {
					t.Fatal("connection configuration changed")
				}
				return tc.handle, tc.err
			})
			if called != tc.wantOpen || (err != nil) != tc.wantError {
				t.Fatalf("called=%v err=%v", called, err)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Fatal("lost startup failure cause")
			}
			if !tc.wantError && got != connected {
				t.Fatal("live connection not supplied to handlers")
			}
		})
	}
}
