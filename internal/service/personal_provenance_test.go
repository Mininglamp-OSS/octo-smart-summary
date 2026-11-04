package service

import (
	"strings"
	"testing"
)

func TestPersonalSnapshotIdentity(t *testing.T) {
	raw := `{"content_version":2,"scope":{"channel_ids":["private-channel"]}}`
	safe := RebasePersonalSnapshot(&raw, 7, 2, nil)
	if safe == nil || strings.Contains(*safe, "private-channel") {
		t.Fatal("snapshot without a task must lose its scope")
	}
	if !PersonalSnapshotsDiffer(nil, &raw) || !PersonalSnapshotsDiffer(&raw, nil) || PersonalSnapshotsDiffer(nil, nil) {
		t.Fatal("known and unknown history must be distinct")
	}
}
