package main

import (
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"
)

// Keep db.New's optional/test behavior unchanged for other callers. The API
// must not start without the store needed for source authorization.
func openRequiredIMDB(dsn string, open func(string) (*gorm.DB, error)) (*gorm.DB, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("IM_MYSQL_DSN is required")
	}
	handle, err := open(dsn)
	if err != nil {
		return nil, fmt.Errorf("initialize source authorization store: %w", err)
	}
	if handle == nil {
		return nil, errors.New("source authorization store returned no connection")
	}
	return handle, nil
}
