package service

import (
	"encoding/json"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
)

// NormalizePersonalVersionSnapshot repairs the additive-column upgrade case
// without inventing a history entry. A NULL history snapshot means unknown, not
// an edit. Copy current provenance only when identity, payload and the snapshot's
// own version agree; never attach a newer scope to an unrelated historical row.
// Callers hold the task/personal write lock and supply their existing transaction.
func NormalizePersonalVersionSnapshot(tx *gorm.DB, pr model.PersonalResult, version *model.PersonalResultVersion) error {
	if version.SnapshotJSON != nil || pr.SnapshotJSON == nil || pr.TaskID != version.TaskID || pr.UserID != version.UserID || !PersonalVersionPayloadMatches(pr, *version) {
		return nil
	}
	if pr.CurrentVersionID != nil && *pr.CurrentVersionID != version.ID {
		return nil
	}
	var snapshot model.Snapshot
	if json.Unmarshal([]byte(*pr.SnapshotJSON), &snapshot) != nil || snapshot.TaskID != pr.TaskID || snapshot.ContentVersion != version.Version {
		return nil
	}
	result := tx.Model(&model.PersonalResultVersion{}).Where("id = ? AND task_id = ? AND user_id = ? AND snapshot_json IS NULL", version.ID, pr.TaskID, pr.UserID).Update("snapshot_json", pr.SnapshotJSON)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected > 0 {
		version.SnapshotJSON = pr.SnapshotJSON
	}
	return nil
}

func PersonalVersionPayloadMatches(pr model.PersonalResult, version model.PersonalResultVersion) bool {
	return pr.Content == version.Content && pr.CitationsJSON == version.CitationsJSON && pr.MsgCount == version.MsgCount && pr.TotalTokenUsed == version.TotalTokenUsed && pr.ModelVersion == version.ModelVersion
}

// Compare after normalizing recoverable legacy history in the transaction.
func PersonalSnapshotsDiffer(a, b *string) bool {
	if a == nil || b == nil {
		return a != b
	}
	return *a != *b
}

// RebasePersonalSnapshot labels inherited provenance with the history row that
// now contains it. NULL stays unknown; invalid or foreign provenance must not
// become a fabricated source scope. parentVersion is from the actual history
// parent, never from the potentially stale embedded content_version.
func RebasePersonalSnapshot(raw *string, taskID int64, version int, parentVersion *int) *string {
	if raw == nil {
		return nil
	}
	var snapshot model.Snapshot
	if json.Unmarshal([]byte(*raw), &snapshot) != nil || snapshot.TaskID != taskID {
		return model.UnknownGenerationSnapshot(taskID, version)
	}
	sameParent := snapshot.ParentSnapshotVersion == nil && parentVersion == nil
	if snapshot.ParentSnapshotVersion != nil && parentVersion != nil {
		sameParent = *snapshot.ParentSnapshotVersion == *parentVersion
	}
	if snapshot.ContentVersion == version && sameParent {
		return raw
	}
	snapshot.ContentVersion = version
	snapshot.ParentSnapshotVersion = parentVersion
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return model.UnknownGenerationSnapshot(taskID, version)
	}
	result := string(encoded)
	return &result
}
