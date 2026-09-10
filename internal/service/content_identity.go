package service

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

const (
	ContentResult   = "result"
	ContentPersonal = "personal"
)

// ContentTarget is a stable business identity, independent of run-scoped row
// IDs and independent of which engine produced the content. Tokens are opaque
// encodings, NOT authorization credentials; every query still scopes by Space,
// task and owner after checking the authenticated caller.
type ContentTarget struct {
	SpaceID string `json:"s"`
	TaskID  int64  `json:"t"`
	Kind    string `json:"k"`
	UserID  string `json:"u,omitempty"`
}

func (t ContentTarget) valid() bool {
	return strings.TrimSpace(t.SpaceID) != "" && len(t.SpaceID) <= 64 && t.TaskID > 0 &&
		((t.Kind == ContentResult && t.UserID == "") ||
			(t.Kind == ContentPersonal && strings.TrimSpace(t.UserID) != "" && len(t.UserID) <= 64))
}

func (t ContentTarget) ID() string {
	if !t.valid() {
		return ""
	}
	return encodeContentToken("sc1_", t)
}

func ParseContentID(id string) (ContentTarget, error) {
	var t ContentTarget
	if err := decodeContentToken(id, "sc1_", &t); err != nil || !t.valid() || t.ID() != id {
		return ContentTarget{}, errors.New("invalid content_id")
	}
	return t, nil
}

type ContentVersionIdentity struct {
	Target      ContentTarget `json:"c"`
	RowID       int64         `json:"v"`
	Provisional bool          `json:"p,omitempty"`
	Revision    int64         `json:"r,omitempty"`
	Digest      string        `json:"d,omitempty"`
}

func (v ContentVersionIdentity) ID() string {
	return encodeContentToken("sv1_", v)
}

func ParseContentVersionID(id string, target ContentTarget) (ContentVersionIdentity, error) {
	var v ContentVersionIdentity
	err := decodeContentToken(id, "sv1_", &v)
	if err != nil || v.Target != target || !v.Target.valid() || v.ID() != id {
		return v, errors.New("invalid version_id")
	}
	if v.Provisional {
		if v.Target.Kind != ContentPersonal || v.RowID != 0 || v.Revision <= 0 || len(v.Digest) != 64 {
			return v, errors.New("invalid provisional version_id")
		}
	} else if v.RowID <= 0 || v.Revision != 0 || v.Digest != "" {
		return v, errors.New("invalid version_id")
	}
	return v, nil
}

func encodeContentToken(prefix string, value any) string {
	b, _ := json.Marshal(value) // all token values contain only JSON scalar fields
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

func decodeContentToken(token, prefix string, dst any) error {
	if !strings.HasPrefix(token, prefix) || len(token) > 1536 {
		return errors.New("invalid token")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, prefix))
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("trailing token data")
	}
	return nil
}
