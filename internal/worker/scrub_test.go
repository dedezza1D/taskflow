package worker

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestScrubErrorRedactsPII(t *testing.T) {
	err := fmt.Errorf("ocr failed for john.doe@example.com, card 4111111111111111, iban DE89370400440532013000")
	out := ScrubError(err)

	for _, leaked := range []string{"john.doe@example.com", "4111111111111111", "DE89370400440532013000"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("scrubbed error still contains %q: %s", leaked, out)
		}
	}
	if !strings.Contains(out, "[REDACTED:email]") {
		t.Fatalf("expected redaction marker in: %s", out)
	}
}

func TestScrubErrorBoundsLength(t *testing.T) {
	err := errors.New(strings.Repeat("x", 5000))
	out := ScrubError(err)
	if len(out) > 1100 {
		t.Fatalf("scrubbed error not bounded: %d bytes", len(out))
	}
	if !strings.HasSuffix(out, "…(truncated)") {
		t.Fatalf("expected truncation marker, got tail %q", out[len(out)-20:])
	}
}

func TestScrubErrorNil(t *testing.T) {
	if ScrubError(nil) != "" {
		t.Fatal("nil error should scrub to empty string")
	}
}
