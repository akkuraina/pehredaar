package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestTokensAreRandomAndHashed(t *testing.T) {
	a, ha := NewToken()
	b, _ := NewToken()
	if a == b || len(a) < 40 {
		t.Errorf("tokens %q and %q: want distinct, long", a, b)
	}
	if ha != HashToken(a) || ha == a || len(ha) != 64 {
		t.Errorf("hash %q of %q", ha, a)
	}
}

func TestSignID(t *testing.T) {
	s := NewSigner([]byte(strings.Repeat("k", 32)))
	v := s.SignID("unsub", 42)
	if id, err := s.VerifyID("unsub", v); err != nil || id != 42 {
		t.Fatalf("VerifyID = %d, %v", id, err)
	}
	other := NewSigner([]byte(strings.Repeat("x", 32)))
	for name, bad := range map[string]string{
		"other purpose": "",
		"tampered id":   strings.Replace(v, "42.", "43.", 1),
		"no signature":  "42",
		"other key":     other.SignID("unsub", 42),
	} {
		purpose := "unsub"
		if name == "other purpose" {
			purpose, bad = "session", v
		}
		if _, err := s.VerifyID(purpose, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestSessions(t *testing.T) {
	s := NewSigner([]byte(strings.Repeat("k", 32)))
	now := time.Unix(1_800_000_000, 0)
	s.now = func() time.Time { return now }

	c := s.SignSession(7, time.Hour)
	if id, err := s.VerifySession(c); err != nil || id != 7 {
		t.Fatalf("VerifySession = %d, %v", id, err)
	}
	now = now.Add(2 * time.Hour)
	if _, err := s.VerifySession(c); !errors.Is(err, ErrInvalid) {
		t.Errorf("expired session: err = %v", err)
	}
	now = now.Add(-2 * time.Hour)
	if _, err := s.VerifySession(strings.Replace(c, "7.", "8.", 1)); !errors.Is(err, ErrInvalid) {
		t.Errorf("tampered session accepted")
	}
	// An unsubscribe signature is not a session.
	if _, err := s.VerifySession(s.SignID("unsub", 7)); !errors.Is(err, ErrInvalid) {
		t.Errorf("unsubscribe value accepted as a session")
	}
}

type memSettings map[string]string

func (m memSettings) Setting(_ context.Context, k, init string) (string, error) {
	if v, ok := m[k]; ok {
		return v, nil
	}
	m[k] = init
	return init, nil
}

func TestLoadSignerKeepsKey(t *testing.T) {
	st := memSettings{}
	a, err := LoadSigner(t.Context(), st)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := LoadSigner(t.Context(), st)
	if id, err := b.VerifyID("p", a.SignID("p", 1)); err != nil || id != 1 {
		t.Error("a reloaded signer can't verify what the first one signed")
	}
}
