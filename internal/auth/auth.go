// Package auth holds Pehredaar's small amount of cryptography: one-time
// tokens for sign-in and email confirmation, and HMAC-signed values for
// session cookies and unsubscribe links.
//
// There are no passwords. A person proves who they are by clicking a link
// sent to an address (or a Telegram chat) they control.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrInvalid is returned for any value that fails verification. It never
// says why (bad signature, expired, malformed), so it can't help an
// attacker probe.
var ErrInvalid = errors.New("auth: invalid or expired")

// NewToken returns a random URL-safe token and the hash to store for it.
// The token goes in the link; only the hash goes in the database, so a
// copy of the database can't be used to sign in.
func NewToken() (token, hash string) {
	b := make([]byte, 32)
	// crypto/rand.Read never fails on supported platforms (Go 1.24+ panics
	// instead of returning an error), so the error is not checked.
	rand.Read(b)
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token)
}

// HashToken is the stored form of a token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Settings is the part of the store that persists the signing key.
type Settings interface {
	Setting(ctx context.Context, key, init string) (string, error)
}

// Signer makes and checks HMAC-signed strings. Create it with LoadSigner.
type Signer struct {
	key []byte
	now func() time.Time
}

// LoadSigner returns a Signer whose key is generated on first use and kept
// in the database, so sessions and unsubscribe links survive restarts.
// Deleting the database (or the setting) signs everyone out and voids old
// unsubscribe links, which is acceptable for a key rotation.
func LoadSigner(ctx context.Context, st Settings) (*Signer, error) {
	fresh := make([]byte, 32)
	rand.Read(fresh)
	v, err := st.Setting(ctx, "signing-key", hex.EncodeToString(fresh))
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(v)
	if err != nil || len(key) < 32 {
		return nil, fmt.Errorf("auth: stored signing key is malformed")
	}
	return NewSigner(key), nil
}

// NewSigner returns a Signer with the given key (tests use a fixed one).
func NewSigner(key []byte) *Signer {
	return &Signer{key: key, now: time.Now}
}

// mac signs purpose and value together, so a signature made for one
// purpose (say, unsubscribe) can't be replayed as another (a session).
func (s *Signer) mac(purpose, value string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(purpose + "\x00" + value))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// SignID returns "<id>.<signature>": a value that never expires, used for
// one-click unsubscribe links that must keep working in old emails.
func (s *Signer) SignID(purpose string, id int64) string {
	v := strconv.FormatInt(id, 10)
	return v + "." + s.mac(purpose, v)
}

// VerifyID checks a value made by SignID and returns its id.
func (s *Signer) VerifyID(purpose, signed string) (int64, error) {
	v, sig, ok := strings.Cut(signed, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(s.mac(purpose, v))) {
		return 0, ErrInvalid
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id <= 0 {
		return 0, ErrInvalid
	}
	return id, nil
}

// SignSession returns "<id>.<expiry unix>.<signature>" for a session
// cookie. Stateless: there is no sessions table to clean up.
func (s *Signer) SignSession(subscriberID int64, ttl time.Duration) string {
	v := strconv.FormatInt(subscriberID, 10) + "." + strconv.FormatInt(s.now().Add(ttl).Unix(), 10)
	return v + "." + s.mac("session", v)
}

// VerifySession checks a session cookie and returns the subscriber id.
func (s *Signer) VerifySession(cookie string) (int64, error) {
	i := strings.LastIndexByte(cookie, '.')
	if i < 0 {
		return 0, ErrInvalid
	}
	v, sig := cookie[:i], cookie[i+1:]
	if !hmac.Equal([]byte(sig), []byte(s.mac("session", v))) {
		return 0, ErrInvalid
	}
	idStr, expStr, ok := strings.Cut(v, ".")
	if !ok {
		return 0, ErrInvalid
	}
	id, err1 := strconv.ParseInt(idStr, 10, 64)
	exp, err2 := strconv.ParseInt(expStr, 10, 64)
	if err1 != nil || err2 != nil || id <= 0 || s.now().Unix() >= exp {
		return 0, ErrInvalid
	}
	return id, nil
}
