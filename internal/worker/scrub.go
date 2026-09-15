package worker

import "github.com/dedezza1D/taskflow/internal/pii"

// ScrubError returns a handler error rendered safe to persist or log.
//
// It is the single chokepoint for keeping document content / PII out of durable
// stores (task_executions.error), the DLQ stream, logs (shipped to Loki), and
// spans (exported to Tempo) — the C2/C3 discipline for the compliance pipeline:
// a handler must never embed document content in an error string, and this is
// where that rule is enforced rather than trusted.
//
// It applies two treatments, in order:
//
//  1. pattern redaction: the SAME validated detector set the PII stage uses
//     (internal/pii — Luhn cards, mod-97 IBANs, check-digit Steuer-IDs, emails,
//     best-effort phones) replaces values with [REDACTED:<category>], so the
//     stage's detection power and the audit trail's scrubbing power never
//     drift apart; and
//  2. a hard length cap, bounding error blobs in the DB and logs.
//
// Honest scope: redaction is exactly as strong as the detector set — free-text
// PII a regex cannot see (names, addresses) still requires the handler-side
// rule of never embedding document content in errors. The Presidio NER upgrade
// strengthens both sides at once, here and in the PII stage, because they share
// internal/pii.
func ScrubError(err error) string {
	if err == nil {
		return ""
	}
	const maxLen = 1024
	s := pii.Redact(err.Error())
	if len(s) > maxLen {
		return s[:maxLen] + "…(truncated)"
	}
	return s
}
