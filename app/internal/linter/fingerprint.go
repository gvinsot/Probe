package linter

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/gvinsot/Probe/app/internal/model"
)

// TokenDigest returns the hex SHA-256 of the Go token stream of src. Comments
// and formatting are ignored, literal values and operators are kept, and
// semicolons (explicit or automatically inserted) carry no literal, so
// sources that differ only in comments or in spacing within lines share a
// digest; joining or splitting lines can change it where Go inserts
// semicolons. It is a syntactic fingerprint, not a statement about behavior.
func TokenDigest(src []byte) string {
	sum := sha256.Sum256([]byte(bodyTokens(string(src))))
	return hex.EncodeToString(sum[:])
}

// FirstChangedLine returns the line and side ("new" or "old") of the first
// added or deleted line of f, the anchor of file-level signals: line 1 on the
// old side of a deleted file, and line 1 on the new side otherwise.
func FirstChangedLine(f model.ChangedFile) (int, string) { return firstChangedLine(f) }
