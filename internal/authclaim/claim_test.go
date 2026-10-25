package authclaim

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestFormatAndNormalize(t *testing.T) {
	if got := Format("abcd-efgh"); got != "ABCD-EFGH" {
		t.Fatalf("Format() = %q, want ABCD-EFGH", got)
	}
	if got := Format("abcdefgh"); got != "ABCD-EFGH" {
		t.Fatalf("Format() = %q, want ABCD-EFGH", got)
	}
	if got := Normalize("  ab-cd efgh \n"); got != "ABCDEFGH" {
		t.Fatalf("Normalize() = %q, want ABCDEFGH", got)
	}
	if got := Normalize("ab_cd-efgh"); got != "ABCDEFGH" {
		t.Fatalf("Normalize() should ignore underscores, got %q", got)
	}
}

func TestGenerateUsesUnambiguousAlphabet(t *testing.T) {
	m := NewManager()
	seen := make(map[byte]bool)
	for i := 0; i < 200; i++ {
		code, expiresAt, err := m.Generate()
		if err != nil {
			t.Fatalf("Generate() error: %v", err)
		}
		if len(code) != CodeLength {
			t.Fatalf("code length = %d, want %d", len(code), CodeLength)
		}
		if got := Format(code); len(got) != 9 || got[4] != '-' {
			t.Fatalf("Format(%q) = %q, want XXXX-XXXX", code, got)
		}
		if !expiresAt.After(time.Now()) {
			t.Fatalf("expiry %v is not in the future", expiresAt)
		}
		for i := 0; i < len(code); i++ {
			if !strings.ContainsRune(alphabet, rune(code[i])) {
				t.Fatalf("code %q contains character outside alphabet: %q", code, code[i])
			}
			for _, ambiguous := range []byte{'0', 'O', '1', 'I', 'L', 'U'} {
				if code[i] == ambiguous {
					t.Fatalf("code %q contains ambiguous character %q", code, ambiguous)
				}
			}
			seen[code[i]] = true
		}
	}
	// 200 枚码 × 8 位，30 字符字母表几乎必然全部覆盖。
	if len(seen) < len(alphabet)-2 {
		t.Fatalf("generated codes only cover %d of %d alphabet characters", len(seen), len(alphabet))
	}
}

func TestVerifyAcceptsHyphenAndCaseAndConsumesCode(t *testing.T) {
	m := NewManager()
	code, _, err := m.Generate()
	if err != nil {
		t.Fatal(err)
	}

	input := strings.ToLower(Format(code))
	if _, err := m.Verify(input); err != nil {
		t.Fatalf("Verify(%q) error: %v", input, err)
	}

	// 成功后旧码立即作废，无法重放。
	if _, err := m.Verify(code); !errors.Is(err, ErrNoActiveCode) {
		t.Fatalf("second Verify() error = %v, want ErrNoActiveCode", err)
	}
}

func TestVerifyExpiresAndRequiresRestart(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	m := NewManager(WithClock(func() time.Time { return now }))
	code, _, err := m.Generate()
	if err != nil {
		t.Fatal(err)
	}

	now = now.Add(TTL + time.Second)
	if _, err := m.Verify(code); !errors.Is(err, ErrExpired) {
		t.Fatalf("Verify() error = %v, want ErrExpired", err)
	}
	// 过期码已清空；再次尝试也不会自动生成新码，只能重启。
	if _, err := m.Verify(code); !errors.Is(err, ErrNoActiveCode) {
		t.Fatalf("Verify() after expiry error = %v, want ErrNoActiveCode", err)
	}
}

func TestFourFailuresKeepCodeValid(t *testing.T) {
	m := NewManager()
	code, _, err := m.Generate()
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < MaxFailures-1; i++ {
		if _, err := m.Verify("WRONGCODE"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("failure %d error = %v, want ErrInvalid", i+1, err)
		}
	}

	if _, err := m.Verify(code); err != nil {
		t.Fatalf("correct code should still be accepted after 4 failures, got %v", err)
	}
}

func TestFifthFailureRotatesCode(t *testing.T) {
	m := NewManager()
	oldCode, _, err := m.Generate()
	if err != nil {
		t.Fatal(err)
	}

	var rotated string
	for i := 0; i < MaxFailures; i++ {
		rotated, err = m.Verify("WRONGCODE")
		if i < MaxFailures-1 {
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("failure %d error = %v, want ErrInvalid", i+1, err)
			}
			continue
		}
		if !errors.Is(err, ErrTooManyFailures) {
			t.Fatalf("5th failure error = %v, want ErrTooManyFailures", err)
		}
	}

	if len(rotated) != CodeLength {
		t.Fatalf("rotated code = %q, want %d characters", rotated, CodeLength)
	}
	if rotated == oldCode {
		t.Fatalf("rotated code must differ from the invalidated code")
	}

	// 旧码已作废，新码立即可用。
	if _, err := m.Verify(oldCode); !errors.Is(err, ErrInvalid) {
		t.Fatalf("old code error = %v, want ErrInvalid", err)
	}
	if _, err := m.Verify(Format(rotated)); err != nil {
		t.Fatalf("rotated code should be accepted, got %v", err)
	}
}

func TestGenerateResetsFailureCounter(t *testing.T) {
	m := NewManager()
	if _, _, err := m.Generate(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxFailures-1; i++ {
		if _, err := m.Verify("WRONGCODE"); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}

	code, _, err := m.Generate()
	if err != nil {
		t.Fatal(err)
	}
	// 重新生成后计数归零：再错 4 次仍然有效。
	for i := 0; i < MaxFailures-1; i++ {
		if _, err := m.Verify("WRONGCODE"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("failure %d after regenerate = %v, want ErrInvalid", i+1, err)
		}
	}
	if _, err := m.Verify(code); err != nil {
		t.Fatalf("fresh code should survive 4 failures, got %v", err)
	}
}

func TestGenerateRejectsReadFailure(t *testing.T) {
	m := NewManager(WithRand(bytes.NewReader(nil)))
	if _, _, err := m.Generate(); err == nil {
		t.Fatal("Generate() should fail when the random source is exhausted")
	}
}
