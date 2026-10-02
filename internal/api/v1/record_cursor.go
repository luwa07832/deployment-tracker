package v1

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"

	"github.com/luwa07832/deployment-tracker/internal/store"
)

// releaseRecordCursorKey seals opaque list cursors so the internal keyset
// position (recorded_at and the row primary key) never reaches callers in a
// readable or forgeable form. The key is process-private randomness: cursors
// only need to be understood by the process that minted them.
var releaseRecordCursorKey = newReleaseRecordCursorKey()

func newReleaseRecordCursorKey() []byte {
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		panic(err)
	}
	return key
}

// releaseRecordCursor is the plaintext sealed inside the opaque token. The
// filter snapshot binds the cursor to the normalized query parameters of the
// request that minted it, so a cursor cannot be replayed against a different
// query; the position is the keyset anchor of the last row returned.
type releaseRecordCursor struct {
	Filter releaseCursorFilter `json:"f"`
	Pos    releaseCursorPos    `json:"p"`
}

type releaseCursorFilter struct {
	Environment string `json:"environment"`
	Version     string `json:"version"`
	BatchID     string `json:"batch_id"`
	GateStatus  string `json:"gate_status"`
	From        string `json:"from"`
	To          string `json:"to"`
}

type releaseCursorPos struct {
	RecordedAt string `json:"recorded_at"`
	ID         int64  `json:"id"`
}

func releaseFilterCursor(filter store.ReleaseRecordFilter) releaseCursorFilter {
	return releaseCursorFilter{
		Environment: filter.Environment,
		Version:     filter.Version,
		BatchID:     filter.BatchID,
		GateStatus:  filter.GateStatus,
		From:        filter.From,
		To:          filter.To,
	}
}

// encodeReleaseRecordCursor seals the cursor payload with AES-GCM and returns
// a URL-safe opaque token.
func encodeReleaseRecordCursor(cursor releaseRecordCursor) (string, error) {
	block, err := aes.NewCipher(releaseRecordCursorKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, payload, nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// decodeReleaseRecordCursor opens an opaque token, verifies it was minted for
// the given normalized filter snapshot and returns its keyset position. Any
// truncation, tampering, wrong key or filter mismatch yields ok == false.
func decodeReleaseRecordCursor(raw string, expected releaseCursorFilter) (releaseCursorPos, bool) {
	sealed, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return releaseCursorPos{}, false
	}
	block, err := aes.NewCipher(releaseRecordCursorKey)
	if err != nil {
		return releaseCursorPos{}, false
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return releaseCursorPos{}, false
	}
	if len(sealed) < gcm.NonceSize() {
		return releaseCursorPos{}, false
	}
	nonce, ciphertext := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	payload, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return releaseCursorPos{}, false
	}
	var cursor releaseRecordCursor
	if err := json.Unmarshal(payload, &cursor); err != nil {
		return releaseCursorPos{}, false
	}
	if cursor.Filter != expected {
		return releaseCursorPos{}, false
	}
	if cursor.Pos.RecordedAt == "" || cursor.Pos.ID <= 0 {
		return releaseCursorPos{}, false
	}
	return cursor.Pos, true
}
