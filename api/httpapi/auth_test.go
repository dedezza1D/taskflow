package httpapi

// Authentication and authorisation tests. These drive the real router, so they
// cover the middleware chain and the per-route role wiring — the place a
// mistake silently opens an endpoint.
//
// Needs Postgres with the migrations applied, same convention as the other
// integration tests in this package.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"

	"github.com/dedezza1D/taskflow/internal/auth"
	"github.com/dedezza1D/taskflow/internal/objects"
	"github.com/dedezza1D/taskflow/internal/pipeline"
	"github.com/dedezza1D/taskflow/internal/store"
	"github.com/dedezza1D/taskflow/internal/testdb"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// newAuthTestServer builds a server with auth ON and a cookie-keeping client.
func newAuthTestServer(t *testing.T) (base string, client *http.Client, st *store.Store) {
	t.Helper()

	st = mustStore(t)
	t.Cleanup(st.Close)

	fs, err := objects.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("objects.NewFS: %v", err)
	}

	srv := NewServer(Config{
		Port:        "0",
		Objects:     fs,
		AuthEnabled: true,
	}, zap.NewNop(), st, nil)

	ts := httptest.NewServer(srv.httpServer.Handler)
	t.Cleanup(ts.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return ts.URL, &http.Client{Jar: jar}, st
}

// makeUser creates a user with a unique email so parallel runs never collide.
func makeUser(t *testing.T, st *store.Store, role store.Role) (email, password string) {
	t.Helper()
	email = fmt.Sprintf("%s-%s@example.test", role, uuid.NewString()[:8])
	password = "correct-horse-battery-staple"

	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	user, err := st.CreateUser(context.Background(), store.CreateUserParams{
		OrgID:        auth.LocalOrgID,
		Email:        email,
		PasswordHash: hash,
		Role:         role,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() { _ = st.DeleteUser(context.Background(), user.ID) })
	return email, password
}

// createOtherOrg creates a second tenant and removes it when the test ends.
//
// Registered before anything the test puts inside the organisation, so it runs
// last (t.Cleanup is LIFO): documents do not cascade from their organisation on
// purpose — erasure must run first — and those rows are gone by then. Tasks and
// users do cascade. Without this, every run left an "other-xxxxxxxx" row behind.
func createOtherOrg(t *testing.T, st *store.Store) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := st.CreateOrganization(context.Background(), id, "other-"+id.String()[:8]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testdb.Exec(t, `DELETE FROM documents WHERE org_id = $1`, id)
		testdb.Exec(t, `DELETE FROM organizations WHERE id = $1`, id)
	})
	return id
}

func login(t *testing.T, client *http.Client, base, email, password string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(loginRequest{Email: email, Password: password})
	resp, err := client.Post(base+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	return resp
}

func TestLoginSucceedsAndEstablishesSession(t *testing.T) {
	base, client, st := newAuthTestServer(t)
	email, password := makeUser(t, st, store.RoleAnalyst)

	resp := login(t, client, base, email, password)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: expected 200, got %d", resp.StatusCode)
	}

	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == auth.SessionCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie issued")
	}
	if !cookie.HttpOnly {
		t.Error("session cookie is not HttpOnly — readable by any XSS")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Error("session cookie is not SameSite=Lax — the CSRF defence this API relies on")
	}

	// The cookie now authenticates.
	me, err := client.Get(base + "/api/v1/auth/me")
	if err != nil {
		t.Fatal(err)
	}
	defer me.Body.Close()
	if me.StatusCode != http.StatusOK {
		t.Fatalf("/auth/me: expected 200, got %d", me.StatusCode)
	}
	var who meResponse
	if err := json.NewDecoder(me.Body).Decode(&who); err != nil {
		t.Fatal(err)
	}
	if who.Email != email || who.Role != store.RoleAnalyst {
		t.Fatalf("/auth/me returned %+v, want %s/%s", who, email, store.RoleAnalyst)
	}
}

// The stored token must be a hash: whoever reads the database must not be able
// to replay a session from it.
func TestSessionTokenIsNotStoredInPlaintext(t *testing.T) {
	base, client, st := newAuthTestServer(t)
	email, password := makeUser(t, st, store.RoleViewer)

	resp := login(t, client, base, email, password)
	defer resp.Body.Close()

	var token string
	for _, c := range resp.Cookies() {
		if c.Name == auth.SessionCookieName {
			token = c.Value
		}
	}
	if token == "" {
		t.Fatal("no session token")
	}

	ctx := context.Background()
	if _, err := st.GetSessionUser(ctx, token); err == nil {
		t.Fatal("the raw token resolves a session — it is being stored in plaintext")
	}
	if _, err := st.GetSessionUser(ctx, auth.HashToken(token)); err != nil {
		t.Fatalf("hashed token should resolve the session: %v", err)
	}
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	base, client, st := newAuthTestServer(t)
	email, _ := makeUser(t, st, store.RoleAdmin)

	resp := login(t, client, base, email, "not-the-password")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	if code := decodeErrCode(t, resp); code != "invalid_credentials" {
		t.Fatalf("error code = %q, want invalid_credentials", code)
	}
}

// An unknown address and a wrong password must be indistinguishable, or the
// API becomes a directory of who has an account.
func TestLoginDoesNotRevealWhetherAccountExists(t *testing.T) {
	base, client, st := newAuthTestServer(t)
	email, _ := makeUser(t, st, store.RoleViewer)

	known := login(t, client, base, email, "wrong-password-here")
	defer known.Body.Close()
	knownCode := decodeErrCode(t, known)

	unknown := login(t, client, base, "nobody-"+uuid.NewString()+"@example.test", "wrong-password-here")
	defer unknown.Body.Close()
	unknownCode := decodeErrCode(t, unknown)

	if known.StatusCode != unknown.StatusCode || knownCode != unknownCode {
		t.Fatalf("responses differ: known=%d/%s unknown=%d/%s",
			known.StatusCode, knownCode, unknown.StatusCode, unknownCode)
	}
}

func TestAnonymousIsRejectedFromDocumentRoutes(t *testing.T) {
	base, client, _ := newAuthTestServer(t)

	resp, err := client.Get(base + "/api/v1/documents")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for anonymous listing, got %d", resp.StatusCode)
	}
	if code := decodeErrCode(t, resp); code != "unauthenticated" {
		t.Fatalf("error code = %q, want unauthenticated", code)
	}
}

// Health has to stay open: container healthchecks probe it with no session.
func TestHealthStaysPublic(t *testing.T) {
	base, client, _ := newAuthTestServer(t)

	resp, err := client.Get(base + "/api/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health should be public, got %d", resp.StatusCode)
	}
}

// Erasure is irreversible, so it is admin-only. A viewer being able to reach it
// would be the worst single authorisation bug in this API.
func TestViewerCannotEraseDocument(t *testing.T) {
	base, client, st := newAuthTestServer(t)
	email, password := makeUser(t, st, store.RoleViewer)

	resp := login(t, client, base, email, password)
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodDelete, base+"/api/v1/documents/"+uuid.NewString(), nil)
	del, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer del.Body.Close()

	if del.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer erasing: expected 403, got %d", del.StatusCode)
	}
	if code := decodeErrCode(t, del); code != "forbidden" {
		t.Fatalf("error code = %q, want forbidden", code)
	}
}

func TestViewerCannotUpload(t *testing.T) {
	base, client, st := newAuthTestServer(t)
	email, password := makeUser(t, st, store.RoleViewer)

	resp := login(t, client, base, email, password)
	resp.Body.Close()

	ct := "text/plain"
	body, formCT := uploadBody(t, "file", "x.txt", &ct, []byte("hello"))
	up := postUpload(t, client, base, body, formCT)
	defer up.Body.Close()

	if up.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer uploading: expected 403, got %d", up.StatusCode)
	}
}

func TestAnalystCannotManageUsers(t *testing.T) {
	base, client, st := newAuthTestServer(t)
	email, password := makeUser(t, st, store.RoleAnalyst)

	resp := login(t, client, base, email, password)
	resp.Body.Close()

	list, err := client.Get(base + "/api/v1/users")
	if err != nil {
		t.Fatal(err)
	}
	defer list.Body.Close()
	if list.StatusCode != http.StatusForbidden {
		t.Fatalf("analyst listing users: expected 403, got %d", list.StatusCode)
	}
}

func TestLogoutRevokesSessionServerSide(t *testing.T) {
	base, client, st := newAuthTestServer(t)
	email, password := makeUser(t, st, store.RoleAnalyst)

	resp := login(t, client, base, email, password)
	var token string
	for _, c := range resp.Cookies() {
		if c.Name == auth.SessionCookieName {
			token = c.Value
		}
	}
	resp.Body.Close()

	out, err := client.Post(base+"/api/v1/auth/logout", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	out.Body.Close()

	// The row is gone, so a copy of the token taken before logout is useless.
	if _, err := st.GetSessionUser(context.Background(), auth.HashToken(token)); err == nil {
		t.Fatal("session still resolves after logout — it was only cleared client-side")
	}
}

// Documents belong to an organisation, and a document from another tenant must
// read as absent rather than forbidden, so ids cannot be probed for existence.
func TestDocumentsAreScopedToOrganization(t *testing.T) {
	base, client, st := newAuthTestServer(t)
	ctx := context.Background()

	// A second organisation with a document of its own.
	otherOrg := createOtherOrg(t, st)
	foreign, err := st.CreateDocument(ctx, store.CreateDocumentParams{
		Filename:    "theirs.txt",
		ContentType: "text/plain",
		StorageURI:  "fs://documents/theirs/original",
		OrgID:       otherOrg,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.DeleteDocument(ctx, foreign.ID) })

	email, password := makeUser(t, st, store.RoleAdmin) // in LocalOrgID
	resp := login(t, client, base, email, password)
	resp.Body.Close()

	get, err := client.Get(base + "/api/v1/documents/" + foreign.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	defer get.Body.Close()
	if get.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-tenant read: expected 404, got %d", get.StatusCode)
	}

	// And it must not appear in the listing either.
	list, err := client.Get(base + "/api/v1/documents?limit=200")
	if err != nil {
		t.Fatal(err)
	}
	defer list.Body.Close()
	var listed listDocumentsResponse
	if err := json.NewDecoder(list.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	for _, d := range listed.Items {
		if d.ID == foreign.ID {
			t.Fatal("another organisation's document appeared in the listing")
		}
	}
}

// Tasks are an index of their tenant's documents — ids, content types, status,
// per-attempt errors — so they must be scoped exactly like the documents are.
// This once leaked: every signed-in user could list every organisation's tasks.
func TestTasksAreScopedToOrganization(t *testing.T) {
	base, client, st := newAuthTestServer(t)
	ctx := context.Background()

	otherOrg := createOtherOrg(t, st)
	foreign, err := st.CreateTask(ctx, store.CreateTaskParams{
		Type:     pipeline.TaskType,
		Payload:  []byte(`{"document_id":"` + uuid.NewString() + `"}`),
		Priority: store.PriorityNormal,
		OrgID:    otherOrg,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.DeleteTask(ctx, foreign.ID) })

	// A viewer: the lowest role that can read tasks at all.
	email, password := makeUser(t, st, store.RoleViewer) // in LocalOrgID
	login(t, client, base, email, password).Body.Close()

	for _, path := range []string{
		"/api/v1/tasks/" + foreign.ID.String(),
		"/api/v1/tasks/" + foreign.ID.String() + "/executions",
	} {
		resp, err := client.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s across tenants: expected 404, got %d", path, resp.StatusCode)
		}
	}

	list, err := client.Get(base + "/api/v1/tasks?limit=200&type=" + pipeline.TaskType)
	if err != nil {
		t.Fatal(err)
	}
	defer list.Body.Close()
	var listed listTasksResponse
	if err := json.NewDecoder(list.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	for _, task := range listed.Items {
		if task.ID == foreign.ID {
			t.Fatal("another organisation's task appeared in the listing")
		}
	}
}

// POST /tasks must not mint pipeline tasks. Their payload names a document by
// id, and only POST /documents checks that the document is the caller's; a
// hand-built one could point the worker at another tenant's document.
func TestCreateTaskRejectsPipelineType(t *testing.T) {
	base, client, st := newAuthTestServer(t)
	email, password := makeUser(t, st, store.RoleAnalyst)
	login(t, client, base, email, password).Body.Close()

	body := `{"type":"` + pipeline.TaskType + `","payload":{"document_id":"` + uuid.NewString() + `"}}`
	resp, err := client.Post(base+"/api/v1/tasks", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("pipeline task through POST /tasks: expected 400, got %d", resp.StatusCode)
	}

	// Ordinary tasks still work, and land in the caller's own tenant.
	ok, err := client.Post(base+"/api/v1/tasks", "application/json",
		bytes.NewReader([]byte(`{"type":"demo","payload":{}}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer ok.Body.Close()
	if ok.StatusCode != http.StatusCreated {
		t.Fatalf("demo task: expected 201, got %d", ok.StatusCode)
	}
	var created createTaskResponse
	if err := json.NewDecoder(ok.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.DeleteTask(context.Background(), created.Task.ID) })
	if _, err := st.GetTaskForOrg(context.Background(), created.Task.ID, auth.LocalOrgID); err != nil {
		t.Fatalf("created task is not in the caller's organisation: %v", err)
	}
}

// An admin resetting someone else's password must also end that person's
// sessions: a reset prompted by a suspected compromise is worthless if the
// intruder stays signed in.
func TestAdminResetRevokesTargetSessions(t *testing.T) {
	base, adminClient, st := newAuthTestServer(t)

	targetEmail, targetPassword := makeUser(t, st, store.RoleAnalyst)
	adminEmail, adminPassword := makeUser(t, st, store.RoleAdmin)

	// The target signs in and holds a live session.
	jar, _ := cookiejar.New(nil)
	targetClient := &http.Client{Jar: jar}
	resp := login(t, targetClient, base, targetEmail, targetPassword)
	resp.Body.Close()

	check, err := targetClient.Get(base + "/api/v1/auth/me")
	if err != nil {
		t.Fatal(err)
	}
	check.Body.Close()
	if check.StatusCode != http.StatusOK {
		t.Fatalf("target should start signed in, got %d", check.StatusCode)
	}

	// The admin resets it.
	resp = login(t, adminClient, base, adminEmail, adminPassword)
	resp.Body.Close()

	users, err := st.ListUsers(context.Background(), auth.LocalOrgID)
	if err != nil {
		t.Fatal(err)
	}
	var targetID string
	for _, u := range users {
		if u.Email == targetEmail {
			targetID = u.ID.String()
		}
	}
	if targetID == "" {
		t.Fatal("target user not found")
	}

	newPassword := "a-freshly-issued-passphrase"
	body, _ := json.Marshal(resetPasswordRequest{Password: newPassword})
	reset, err := adminClient.Post(
		base+"/api/v1/users/"+targetID+"/password", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer reset.Body.Close()
	if reset.StatusCode != http.StatusNoContent {
		t.Fatalf("reset: expected 204, got %d", reset.StatusCode)
	}

	// The old session is dead...
	after, err := targetClient.Get(base + "/api/v1/auth/me")
	if err != nil {
		t.Fatal(err)
	}
	after.Body.Close()
	if after.StatusCode != http.StatusUnauthorized {
		t.Fatalf("target session survived the reset: got %d", after.StatusCode)
	}

	// ...the old password no longer works...
	old := login(t, targetClient, base, targetEmail, targetPassword)
	old.Body.Close()
	if old.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old password still works after reset: got %d", old.StatusCode)
	}

	// ...and the new one does.
	fresh := login(t, targetClient, base, targetEmail, newPassword)
	defer fresh.Body.Close()
	if fresh.StatusCode != http.StatusOK {
		t.Fatalf("new password should sign in, got %d", fresh.StatusCode)
	}
}

// Resetting your own password must go through /auth/password, which demands the
// current one — otherwise a hijacked admin session becomes a permanent takeover.
func TestAdminCannotResetOwnPasswordViaAdminRoute(t *testing.T) {
	base, client, st := newAuthTestServer(t)
	email, password := makeUser(t, st, store.RoleAdmin)

	resp := login(t, client, base, email, password)
	resp.Body.Close()

	users, err := st.ListUsers(context.Background(), auth.LocalOrgID)
	if err != nil {
		t.Fatal(err)
	}
	var selfID string
	for _, u := range users {
		if u.Email == email {
			selfID = u.ID.String()
		}
	}

	body, _ := json.Marshal(resetPasswordRequest{Password: "trying-to-skip-the-check"})
	got, err := client.Post(base+"/api/v1/users/"+selfID+"/password", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()

	if got.StatusCode != http.StatusBadRequest {
		t.Fatalf("self-reset via the admin route should be refused, got %d", got.StatusCode)
	}
}

func TestAnalystCannotResetPasswords(t *testing.T) {
	base, client, st := newAuthTestServer(t)
	email, password := makeUser(t, st, store.RoleAnalyst)
	victimEmail, _ := makeUser(t, st, store.RoleAdmin)

	resp := login(t, client, base, email, password)
	resp.Body.Close()

	users, _ := st.ListUsers(context.Background(), auth.LocalOrgID)
	var victimID string
	for _, u := range users {
		if u.Email == victimEmail {
			victimID = u.ID.String()
		}
	}

	body, _ := json.Marshal(resetPasswordRequest{Password: "escalating-privileges"})
	got, err := client.Post(base+"/api/v1/users/"+victimID+"/password", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()

	if got.StatusCode != http.StatusForbidden {
		t.Fatalf("an analyst resetting an admin's password must be 403, got %d", got.StatusCode)
	}
}

// A short password must be refused here too, or the reset route becomes a way
// around the length floor that account creation enforces.
func TestResetEnforcesPasswordLength(t *testing.T) {
	base, client, st := newAuthTestServer(t)
	targetEmail, _ := makeUser(t, st, store.RoleViewer)
	adminEmail, adminPassword := makeUser(t, st, store.RoleAdmin)

	resp := login(t, client, base, adminEmail, adminPassword)
	resp.Body.Close()

	users, _ := st.ListUsers(context.Background(), auth.LocalOrgID)
	var targetID string
	for _, u := range users {
		if u.Email == targetEmail {
			targetID = u.ID.String()
		}
	}

	body, _ := json.Marshal(resetPasswordRequest{Password: "short"})
	got, err := client.Post(base+"/api/v1/users/"+targetID+"/password", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()

	if got.StatusCode != http.StatusBadRequest {
		t.Fatalf("a too-short password must be refused, got %d", got.StatusCode)
	}
}

// With auth disabled every request is a local admin. This is the desktop build's
// contract, and the test exists so nobody "fixes" the disabled path into
// rejecting requests.
func TestAuthDisabledRunsAsLocalAdmin(t *testing.T) {
	st := mustStore(t)
	defer st.Close()

	fs, err := objects.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(Config{Port: "0", Objects: fs, AuthEnabled: false}, zap.NewNop(), st, nil)
	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/api/v1/auth/me")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 without a session, got %d", resp.StatusCode)
	}

	var who meResponse
	if err := json.NewDecoder(resp.Body).Decode(&who); err != nil {
		t.Fatal(err)
	}
	if !who.Local || who.Role != store.RoleAdmin {
		t.Fatalf("expected the local admin principal, got %+v", who)
	}
}
