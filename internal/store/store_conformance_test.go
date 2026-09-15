package store

// Conformance suite: one set of cases, run against every backend by
// eachBackend. The store methods are written once in PostgreSQL idiom and are
// expected to behave identically on SQLite; that expectation is only worth
// anything while both are actually exercised. See backends_test.go for what
// happened the last time they were not.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestTaskLifecycle(t *testing.T) {
	eachBackend(t, func(t *testing.T, st *Store) {
		ctx := context.Background()

		task, err := st.CreateTask(ctx, CreateTaskParams{
			Type:     conformanceTaskType,
			Payload:  json.RawMessage(`{"hello":"world"}`),
			Priority: PriorityHigh,
			OrgID:    seedOrg(t, st),
		})
		if err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		if task.Status != StatusQueued || task.Version != 1 {
			t.Fatalf("fresh task wrong: %+v", task)
		}
		// Semantic equality, not byte equality. PostgreSQL stores this column as
		// jsonb, which re-serialises: whitespace is normalised and object keys
		// are reordered. SQLite keeps the text exactly as written. Both are fine
		// because every consumer unmarshals the payload, but it means nothing may
		// ever depend on its exact bytes -- a signature or checksum over a task
		// payload would verify on the desktop build and fail on the served one.
		var round map[string]any
		if err := json.Unmarshal(task.Payload, &round); err != nil {
			t.Fatalf("payload is not valid json after the round trip: %v", err)
		}
		if round["hello"] != "world" {
			t.Fatalf("payload did not round-trip: %s", task.Payload)
		}
		if task.CreatedAt.IsZero() {
			t.Fatal("created_at is zero — the TIMESTAMP column is not producing a time")
		}

		got, err := st.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if got.ID != task.ID {
			t.Fatalf("uuid did not round-trip: %v vs %v", got.ID, task.ID)
		}

		if _, err := st.GetTask(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing task should be ErrNotFound, got %v", err)
		}
	})
}

// Tasks carry their tenant. The request-path reads must hide another
// organisation's task entirely — it is an index of that tenant's documents —
// while the engine's reads still see everything it has to process.
func TestTasksAreScopedToOrganization(t *testing.T) {
	eachBackend(t, func(t *testing.T, st *Store) {
		ctx := context.Background()
		mine, theirs := seedOrg(t, st), seedOrg(t, st)

		own, err := st.CreateTask(ctx, CreateTaskParams{
			Type: conformanceTaskType, Payload: json.RawMessage(`{}`), Priority: PriorityNormal, OrgID: mine,
		})
		if err != nil {
			t.Fatal(err)
		}
		foreign, err := st.CreateTask(ctx, CreateTaskParams{
			Type: conformanceTaskType, Payload: json.RawMessage(`{}`), Priority: PriorityNormal, OrgID: theirs,
		})
		if err != nil {
			t.Fatal(err)
		}

		if _, err := st.GetTaskForOrg(ctx, own.ID, mine); err != nil {
			t.Fatalf("own task: %v", err)
		}
		if _, err := st.GetTaskForOrg(ctx, foreign.ID, mine); !errors.Is(err, ErrNotFound) {
			t.Fatalf("another tenant's task must be ErrNotFound, got %v", err)
		}

		typ := conformanceTaskType
		listed, err := st.ListTasksForOrg(ctx, mine, ListTasksParams{Type: &typ, Limit: 200})
		if err != nil {
			t.Fatalf("ListTasksForOrg: %v", err)
		}
		if len(listed) != 1 || listed[0].ID != own.ID {
			t.Fatalf("scoped listing should hold only the own task, got %+v", listed)
		}

		status := StatusQueued
		scopedByStatus, err := st.ListTasksForOrg(ctx, theirs, ListTasksParams{Status: &status, Type: &typ, Limit: 200})
		if err != nil {
			t.Fatalf("ListTasksForOrg with status: %v", err)
		}
		if len(scopedByStatus) != 1 || scopedByStatus[0].ID != foreign.ID {
			t.Fatalf("status + tenant filters did not combine, got %+v", scopedByStatus)
		}

		if _, err := st.GetTask(ctx, foreign.ID); err != nil {
			t.Fatalf("the engine read must still reach every tenant: %v", err)
		}
		all, err := st.ListTasks(ctx, ListTasksParams{Type: &typ, Limit: 200})
		if err != nil {
			t.Fatal(err)
		}
		if len(all) < 2 {
			t.Fatalf("the engine listing must span tenants, got %d rows", len(all))
		}

		// No tenant, no task: the foreign key refuses an organisation that does
		// not exist, which is what uuid.Nil is.
		if _, err := st.CreateTask(ctx, CreateTaskParams{
			Type: conformanceTaskType, Payload: json.RawMessage(`{}`), Priority: PriorityNormal,
		}); err == nil {
			t.Fatal("a task without an organisation was accepted")
		}
	})
}

// Optimistic locking is the backbone of the pipeline's idempotency. If it
// behaved differently here, retries would corrupt state on the desktop build.
func TestOptimisticLocking(t *testing.T) {
	eachBackend(t, func(t *testing.T, st *Store) {
		ctx := context.Background()

		task, err := st.CreateTask(ctx, CreateTaskParams{
			Type: conformanceTaskType, Payload: json.RawMessage(`{}`), Priority: PriorityNormal, OrgID: seedOrg(t, st),
		})
		if err != nil {
			t.Fatal(err)
		}

		updated, err := st.UpdateTaskStatus(ctx, task.ID, task.Version, StatusProcessing)
		if err != nil {
			t.Fatalf("first update: %v", err)
		}
		if updated.Version != task.Version+1 {
			t.Fatalf("version did not advance: %d -> %d", task.Version, updated.Version)
		}

		// The stale version must lose.
		if _, err := st.UpdateTaskStatus(ctx, task.ID, task.Version, StatusCompleted); !errors.Is(err, ErrVersionConflict) {
			t.Fatalf("stale version should conflict, got %v", err)
		}
	})
}

// The unique (task_id, attempt) index is what makes two racing deliveries
// produce one winner. It has to surface as ErrAlreadyExists here too, which
// means the SQLite constraint code must map to ErrUniqueViolation.
func TestExecutionLedgerRejectsDuplicateAttempt(t *testing.T) {
	eachBackend(t, func(t *testing.T, st *Store) {
		ctx := context.Background()

		task, err := st.CreateTask(ctx, CreateTaskParams{
			Type: conformanceTaskType, Payload: json.RawMessage(`{}`), Priority: PriorityNormal, OrgID: seedOrg(t, st),
		})
		if err != nil {
			t.Fatal(err)
		}

		first, err := st.CreateExecution(ctx, task.ID)
		if err != nil {
			t.Fatalf("first execution: %v", err)
		}
		if first.Attempt != 1 {
			t.Fatalf("first attempt should be 1, got %d", first.Attempt)
		}

		if n, err := st.MaxAttempt(ctx, task.ID); err != nil || n != 1 {
			t.Fatalf("MaxAttempt = %d, %v; want 1, nil", n, err)
		}

		msg := "boom"
		finished, err := st.FinishExecution(ctx, first.ID, ExecFailed, &msg)
		if err != nil {
			t.Fatalf("FinishExecution: %v", err)
		}
		if finished.FinishedAt == nil {
			t.Error("finished_at not set — nullable TIMESTAMP is not round-tripping")
		}
		if finished.Error == nil || *finished.Error != msg {
			t.Errorf("error message did not round-trip: %v", finished.Error)
		}

		second, err := st.CreateExecution(ctx, task.ID)
		if err != nil {
			t.Fatalf("second execution: %v", err)
		}
		if second.Attempt != 2 {
			t.Fatalf("second attempt should be 2, got %d", second.Attempt)
		}
	})
}

func TestDocumentLifecycleAndErasure(t *testing.T) {
	eachBackend(t, func(t *testing.T, st *Store) {
		ctx := context.Background()
		orgID := seedOrg(t, st)

		doc, err := st.CreateDocument(ctx, CreateDocumentParams{
			Filename:    "ficha.txt",
			ContentType: "text/plain",
			StorageURI:  "fs://documents/x/original",
			OrgID:       orgID,
		})
		if err != nil {
			t.Fatalf("CreateDocument: %v", err)
		}
		if doc.Status != DocUploaded {
			t.Fatalf("status = %s, want uploaded", doc.Status)
		}
		if doc.RawShreddedAt != nil {
			t.Fatal("a fresh document should not be marked shredded")
		}

		// Tenant scoping: the same id under another org must read as absent.
		if _, err := st.GetDocumentForOrg(ctx, doc.ID, uuid.New()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-tenant read should be ErrNotFound, got %v", err)
		}
		// Optimistic locking on a document, not just on a task: the second write
		// carries the version it read, which is now stale.
		upd, err := st.UpdateDocumentStatus(ctx, doc.ID, doc.Version, DocProcessing, nil)
		if err != nil {
			t.Fatalf("UpdateDocumentStatus: %v", err)
		}
		if _, err := st.UpdateDocumentStatus(ctx, doc.ID, doc.Version, DocCompleted, nil); !errors.Is(err, ErrVersionConflict) {
			t.Fatalf("stale version should conflict, got %v", err)
		}
		if _, err := st.UpdateDocumentStatus(ctx, doc.ID, upd.Version, DocCompleted, nil); err != nil {
			t.Fatalf("fresh version should win: %v", err)
		}

		if _, err := st.GetDocumentForOrg(ctx, doc.ID, orgID); err != nil {
			t.Fatalf("same-tenant read failed: %v", err)
		}

		// Artifacts are the stage ledger; the unique index must absorb a re-run.
		if _, err := st.CreateArtifact(ctx, doc.ID, "ocr", "text", "fs://documents/x/ocr.txt"); err != nil {
			t.Fatalf("CreateArtifact: %v", err)
		}
		if _, err := st.CreateArtifact(ctx, doc.ID, "ocr", "text", "fs://documents/x/ocr.txt"); err != nil {
			t.Fatalf("idempotent re-create should succeed: %v", err)
		}
		arts, err := st.ListArtifacts(ctx, doc.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(arts) != 1 {
			t.Fatalf("artifact rows = %d, want 1", len(arts))
		}

		if err := st.MarkRawShredded(ctx, doc.ID); err != nil {
			t.Fatalf("MarkRawShredded: %v", err)
		}
		after, err := st.GetDocument(ctx, doc.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.RawShreddedAt == nil {
			t.Fatal("raw_shredded_at not stamped — nullable TIMESTAMP is not round-tripping")
		}

		// Erasure: the artifact rows must cascade, which only happens if the
		// foreign_keys pragma actually took effect.
		if _, err := st.DeleteDocument(ctx, doc.ID); err != nil {
			t.Fatalf("DeleteDocument: %v", err)
		}
		// Erasing twice is a no-op, not an error: a retried request must not
		// turn into a failure the caller has to interpret.
		if deleted, err := st.DeleteDocument(ctx, doc.ID); deleted || err != nil {
			t.Fatalf("second delete should be a no-op, got deleted=%v err=%v", deleted, err)
		}

		if _, err := st.GetDocument(ctx, doc.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("document should be gone, got %v", err)
		}
		orphans, err := st.ListArtifacts(ctx, doc.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(orphans) != 0 {
			t.Fatalf("%d artifact rows survived the cascade — PRAGMA foreign_keys is not on", len(orphans))
		}
	})
}

func TestListDocumentsFiltersByStatusAndOrg(t *testing.T) {
	eachBackend(t, func(t *testing.T, st *Store) {
		ctx := context.Background()
		orgA := seedOrg(t, st)
		orgB := seedOrg(t, st)

		mk := func(org uuid.UUID, name string) *Document {
			d, err := st.CreateDocument(ctx, CreateDocumentParams{
				Filename: name, ContentType: "text/plain",
				StorageURI: "fs://x", OrgID: org,
			})
			if err != nil {
				t.Fatal(err)
			}
			return d
		}

		mine := mk(orgA, "mine.txt")
		mk(orgB, "theirs.txt")

		// The optional-filter form ($1 IS NULL OR status = $1) is the query that
		// needed its Postgres cast removed to work here.
		all, err := st.ListDocuments(ctx, ListDocumentsParams{OrgID: orgA})
		if err != nil {
			t.Fatalf("ListDocuments: %v", err)
		}
		if len(all) != 1 || all[0].ID != mine.ID {
			t.Fatalf("org scoping failed: got %d rows", len(all))
		}

		status := DocUploaded
		filtered, err := st.ListDocuments(ctx, ListDocumentsParams{OrgID: orgA, Status: &status})
		if err != nil {
			t.Fatalf("ListDocuments with status: %v", err)
		}
		if len(filtered) != 1 {
			t.Fatalf("status filter returned %d rows, want 1", len(filtered))
		}

		none := DocCompleted
		empty, err := st.ListDocuments(ctx, ListDocumentsParams{OrgID: orgA, Status: &none})
		if err != nil {
			t.Fatal(err)
		}
		if len(empty) != 0 {
			t.Fatalf("filter on a status nothing has returned %d rows", len(empty))
		}
	})
}

func TestAuthAndSessions(t *testing.T) {
	eachBackend(t, func(t *testing.T, st *Store) {
		ctx := context.Background()
		orgID := seedOrg(t, st)

		user, err := st.CreateUser(ctx, CreateUserParams{
			OrgID: orgID, Email: "dpo@local.test",
			PasswordHash: "hash", Role: RoleAdmin,
		})
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}

		// The UNIQUE(email) breach has to surface as ErrEmailTaken, which depends on
		// ON CONFLICT DO NOTHING plus the no-rows path.
		if _, err := st.CreateUser(ctx, CreateUserParams{
			OrgID: orgID, Email: "dpo@local.test", PasswordHash: "other", Role: RoleViewer,
		}); !errors.Is(err, ErrEmailTaken) {
			t.Fatalf("duplicate email should be ErrEmailTaken, got %v", err)
		}

		found, err := st.GetUserByEmail(ctx, "dpo@local.test")
		if err != nil || found.ID != user.ID {
			t.Fatalf("GetUserByEmail: %v", err)
		}

		// Sessions, including the expiry filter in SQL.
		if err := st.CreateSession(ctx, "livehash", user.ID, time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		if err := st.CreateSession(ctx, "deadhash", user.ID, time.Now().Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}

		if _, err := st.GetSessionUser(ctx, "livehash"); err != nil {
			t.Fatalf("live session should resolve: %v", err)
		}
		if _, err := st.GetSessionUser(ctx, "deadhash"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("expired session must not authenticate, got %v", err)
		}

		// Deleting the user cascades their sessions away.
		if err := st.DeleteUser(ctx, user.ID); err != nil {
			t.Fatalf("DeleteUser: %v", err)
		}
		if _, err := st.GetSessionUser(ctx, "livehash"); !errors.Is(err, ErrNotFound) {
			t.Fatal("session survived its user — the cascade did not fire")
		}
	})
}

// The atomic claim is what stops two clicks on one recovery link from both
// succeeding. It relies on UPDATE ... WHERE used_at IS NULL RETURNING.
func TestPasswordResetTokenIsClaimedOnce(t *testing.T) {
	eachBackend(t, func(t *testing.T, st *Store) {
		ctx := context.Background()
		orgID := seedOrg(t, st)

		user, err := st.CreateUser(ctx, CreateUserParams{
			OrgID: orgID, Email: "user@local.test", PasswordHash: "hash", Role: RoleViewer,
		})
		if err != nil {
			t.Fatal(err)
		}

		if err := st.CreatePasswordResetToken(ctx, "tokhash", user.ID, time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("CreatePasswordResetToken: %v", err)
		}

		claimed, err := st.ConsumePasswordResetToken(ctx, "tokhash")
		if err != nil {
			t.Fatalf("first claim: %v", err)
		}
		if claimed.ID != user.ID {
			t.Fatalf("claim returned the wrong user")
		}

		if _, err := st.ConsumePasswordResetToken(ctx, "tokhash"); !errors.Is(err, ErrTokenUsed) {
			t.Fatalf("second claim should be ErrTokenUsed, got %v", err)
		}
		if _, err := st.ConsumePasswordResetToken(ctx, "nosuchhash"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown token should be ErrNotFound, got %v", err)
		}
	})
}

// Regression: SQLite has no date type, so every timestamp comparison is a
// STRING comparison. Left to itself the driver writes Go's time.String()
// ("2026-09-11 00:19:52.4143168 +0000 UTC") while SQLite's own CURRENT_TIMESTAMP
// writes "2026-09-11 00:19:52" — and the first compares GREATER than the second
// for the same instant, purely because it is longer. That made expired sessions
// authenticate.
//
// This pins the property the fix depends on: for values written through the
// store, string order must equal chronological order.
func TestTimestampOrderMatchesChronology(t *testing.T) {
	eachBackend(t, func(t *testing.T, st *Store) {
		ctx := context.Background()
		orgID := seedOrg(t, st)

		user, err := st.CreateUser(ctx, CreateUserParams{
			OrgID: orgID, Email: "clock@local.test", PasswordHash: "h", Role: RoleViewer,
		})
		if err != nil {
			t.Fatal(err)
		}

		now := time.Now()
		cases := []struct {
			name    string
			expires time.Time
			valid   bool
		}{
			{"long expired", now.Add(-72 * time.Hour), false},
			{"just expired", now.Add(-time.Second), false},
			{"valid soon", now.Add(time.Minute), true},
			{"valid later", now.Add(72 * time.Hour), true},
			// Crosses a date boundary, where a naive prefix comparison is most
			// likely to look plausible and still be wrong.
			{"expired yesterday", now.AddDate(0, 0, -1), false},
		}

		for _, tc := range cases {
			if err := st.CreateSession(ctx, "hash-"+tc.name, user.ID, tc.expires); err != nil {
				t.Fatalf("%s: CreateSession: %v", tc.name, err)
			}
		}

		for _, tc := range cases {
			_, err := st.GetSessionUser(ctx, "hash-"+tc.name)
			switch {
			case tc.valid && err != nil:
				t.Errorf("%s: a live session was rejected: %v", tc.name, err)
			case !tc.valid && err == nil:
				t.Errorf("%s: an EXPIRED session authenticated", tc.name)
			case !tc.valid && !errors.Is(err, ErrNotFound):
				t.Errorf("%s: unexpected error: %v", tc.name, err)
			}
		}

		// The sweep must agree with the lookup: delete exactly the expired ones.
		//
		// Asserted row by row, not by the count it returns. The sweep is global
		// by design, and on PostgreSQL the table is shared with every other test
		// binary running in parallel — a count of 3 only held while nobody else
		// had an expired session lying around, and the suite failed the day
		// someone did.
		n, err := st.DeleteExpiredSessions(ctx)
		if err != nil {
			t.Fatalf("DeleteExpiredSessions: %v", err)
		}
		if n < 3 {
			t.Errorf("sweep deleted %d sessions, want at least this test's 3 expired ones", n)
		}
		for _, tc := range cases {
			var remaining int
			if err := st.db.QueryRow(ctx,
				`SELECT COUNT(*) FROM sessions WHERE token_hash = $1;`, "hash-"+tc.name).Scan(&remaining); err != nil {
				t.Fatalf("%s: count: %v", tc.name, err)
			}
			switch {
			case tc.valid && remaining != 1:
				t.Errorf("%s: sweep removed a session that had not expired", tc.name)
			case !tc.valid && remaining != 0:
				t.Errorf("%s: sweep left an expired session behind", tc.name)
			}
		}
	})
}

func TestRawRetentionSweepQuery(t *testing.T) {
	eachBackend(t, func(t *testing.T, st *Store) {
		ctx := context.Background()
		orgID := seedOrg(t, st)

		doc, err := st.CreateDocument(ctx, CreateDocumentParams{
			Filename: "failed.txt", ContentType: "text/plain",
			StorageURI: "fs://x", OrgID: orgID,
		})
		if err != nil {
			t.Fatal(err)
		}
		stage := "ocr"
		if _, err := st.UpdateDocumentStatus(ctx, doc.ID, doc.Version, DocFailed, &stage); err != nil {
			t.Fatal(err)
		}

		pending, err := st.ListRawShreddablePending(ctx, time.Now().Add(time.Minute), 100)
		if err != nil {
			t.Fatalf("ListRawShreddablePending: %v", err)
		}
		found := false
		for _, d := range pending {
			if d.ID == doc.ID {
				found = true
				if d.FailedStage == nil || *d.FailedStage != "ocr" {
					t.Errorf("failed_stage did not round-trip: %v", d.FailedStage)
				}
			}
		}
		if !found {
			t.Fatal("failed document missing from the sweep")
		}

		if err := st.MarkRawShredded(ctx, doc.ID); err != nil {
			t.Fatal(err)
		}
		again, err := st.ListRawShreddablePending(ctx, time.Now().Add(time.Minute), 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range again {
			if d.ID == doc.ID {
				t.Fatal("shredded document still pending — the sweep would loop forever")
			}
		}
	})
}

// The unfiltered list is the shape the UI actually asks for, and it is what
// PostgreSQL rejected with 42P08 while the SQLite-only suite stayed green:
// "$1 IS NULL" gives it nothing to infer the parameter type from, so it refused
// the statement before ever reaching the comparison that would have told it.
// Both lists were 500s on every served deployment.
//
// Filtered and unfiltered are both asserted, because the fix changed how the
// parameter is typed and must not change what it matches.
func TestListsWithoutFilters(t *testing.T) {
	eachBackend(t, func(t *testing.T, st *Store) {
		ctx := context.Background()
		orgID := seedOrg(t, st)

		doc, err := st.CreateDocument(ctx, CreateDocumentParams{
			Filename:    "a.txt",
			ContentType: "text/plain",
			StorageURI:  "fs://documents/a/original",
			OrgID:       orgID,
		})
		if err != nil {
			t.Fatalf("CreateDocument: %v", err)
		}
		if _, err := st.CreateTask(ctx, CreateTaskParams{
			Type:     conformanceTaskType,
			Payload:  json.RawMessage(`{}`),
			Priority: PriorityNormal,
			OrgID:    orgID,
		}); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}

		docs, err := st.ListDocuments(ctx, ListDocumentsParams{OrgID: orgID, Limit: 10})
		if err != nil {
			t.Fatalf("ListDocuments with no status filter: %v", err)
		}
		if len(docs) != 1 {
			t.Fatalf("expected the one document of this tenant, got %d", len(docs))
		}

		if _, err := st.ListTasks(ctx, ListTasksParams{Limit: 10}); err != nil {
			t.Fatalf("ListTasks with no filters: %v", err)
		}

		status := DocUploaded
		filtered, err := st.ListDocuments(ctx, ListDocumentsParams{OrgID: orgID, Status: &status, Limit: 10})
		if err != nil {
			t.Fatalf("ListDocuments with a status filter: %v", err)
		}
		if len(filtered) != 1 || filtered[0].ID != doc.ID {
			t.Fatalf("status filter did not return the uploaded document: %+v", filtered)
		}

		none := DocErased
		empty, err := st.ListDocuments(ctx, ListDocumentsParams{OrgID: orgID, Status: &none, Limit: 10})
		if err != nil {
			t.Fatalf("ListDocuments with a non-matching filter: %v", err)
		}
		if len(empty) != 0 {
			t.Fatalf("non-matching filter returned %d rows", len(empty))
		}
	})
}
