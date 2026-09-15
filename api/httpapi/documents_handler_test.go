package httpapi

// Handler-level tests for the /documents endpoints. These exercise the upload
// validation ladder (multipart shape, priority, content-type sniffing, size
// ceiling) and the erasure endpoint's idempotency — the code paths the existing
// integration tests never touch.
//
// They need Postgres with the migrations applied, same convention as the other
// *_integration_test.go files here, but drive the router through
// httptest.NewServer rather than a hand-rolled net.Listen.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dedezza1D/taskflow/internal/objects"
	"github.com/dedezza1D/taskflow/internal/store"
	"github.com/dedezza1D/taskflow/internal/testdb"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// mustStore opens the dedicated test database (see internal/testdb) or fails
// the test.
//
// It used to skip. That is worse than it sounds: with no database reachable
// this package reported "ok" having run 4 of its 38 tests -- the entire HTTP
// surface, authentication and password recovery included, quietly not covered
// while the suite stayed green. A test that cannot run has not passed, and the
// only honest way to say so is to fail.
func mustStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(context.Background(), testdb.DSN(t))
	if err != nil {
		t.Fatalf("PostgreSQL is required by these integration tests.\n"+
			"  start it with: docker compose up -d postgres\n"+
			"  cause: %v", err)
	}
	return st
}

// newDocumentsTestServer wires a real store and a temp-dir object store behind
// httptest. objectsRoot is returned so tests can assert on the bytes on disk —
// orphan checks are the whole point of some of these cases.
func newDocumentsTestServer(t *testing.T) (base string, client *http.Client, objectsRoot string) {
	t.Helper()

	st := mustStore(t)
	t.Cleanup(st.Close)

	objectsRoot = t.TempDir()
	fs, err := objects.NewFS(objectsRoot)
	if err != nil {
		t.Fatalf("objects.NewFS: %v", err)
	}

	srv := NewServer(Config{Port: "0", Objects: fs, MaxUploadBytes: 1 << 20}, zap.NewNop(), st, nil)
	ts := httptest.NewServer(srv.httpServer.Handler)
	t.Cleanup(ts.Close)

	return ts.URL, ts.Client(), objectsRoot
}

// uploadBody builds a multipart body. A nil contentType omits the part's
// Content-Type header entirely, which is what forces the sniffing path.
func uploadBody(t *testing.T, field, filename string, contentType *string, data []byte) (io.Reader, string) {
	t.Helper()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	if field != "" {
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition",
			fmt.Sprintf(`form-data; name=%q; filename=%q`, field, filename))
		if contentType != nil {
			h.Set("Content-Type", *contentType)
		}
		part, err := w.CreatePart(h)
		if err != nil {
			t.Fatalf("CreatePart: %v", err)
		}
		if _, err := part.Write(data); err != nil {
			t.Fatalf("write part: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return &buf, w.FormDataContentType()
}

func postUpload(t *testing.T, client *http.Client, base string, body io.Reader, ct string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/documents", body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", ct)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /documents: %v", err)
	}
	return resp
}

func decodeErrCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var payload apiError
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &payload); err != nil {
		t.Fatalf("decode error body %q: %v", string(b), err)
	}
	return payload.Error
}

// countObjects walks the object root — erasure and rollback paths must leave
// zero bytes behind, and "zero" is only meaningful if we count the whole tree.
func countObjects(t *testing.T, root string) int {
	t.Helper()
	n := 0
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk objects: %v", err)
	}
	return n
}

func TestUploadDocument_Success(t *testing.T) {
	base, client, root := newDocumentsTestServer(t)

	ct := "text/plain"
	body, formCT := uploadBody(t, "file", "ficha.txt", &ct, []byte("CPF: 529.982.247-25\n"))
	resp := postUpload(t, client, base, body, formCT)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d body=%s", resp.StatusCode, string(b))
	}

	var created createDocumentResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if created.Document.ID == uuid.Nil {
		t.Fatal("document id is nil")
	}
	if created.Document.ContentType != "text/plain" {
		t.Fatalf("content type = %q, want text/plain", created.Document.ContentType)
	}
	if created.Document.Status != store.DocUploaded {
		t.Fatalf("status = %q, want uploaded", created.Document.Status)
	}
	if created.Document.StorageURI == "" {
		t.Fatal("storage_uri is empty")
	}
	if created.TaskID == "" {
		t.Fatal("task_id is empty")
	}
	if created.Document.TaskID == nil || created.Document.TaskID.String() != created.TaskID {
		t.Fatalf("document.task_id %v does not match task_id %s", created.Document.TaskID, created.TaskID)
	}
	if n := countObjects(t, root); n != 1 {
		t.Fatalf("expected exactly 1 stored object, got %d", n)
	}

	// Clean up the row so repeated runs do not accumulate documents.
	req, _ := http.NewRequest(http.MethodDelete, base+"/api/v1/documents/"+created.Document.ID.String(), nil)
	delResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	delResp.Body.Close()
}

func TestUploadDocument_MissingFileField(t *testing.T) {
	base, client, root := newDocumentsTestServer(t)

	body, formCT := uploadBody(t, "", "", nil, nil)
	resp := postUpload(t, client, base, body, formCT)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	if code := decodeErrCode(t, resp); code != "validation_error" {
		t.Fatalf("error code = %q, want validation_error", code)
	}
	if n := countObjects(t, root); n != 0 {
		t.Fatalf("rejected upload left %d objects behind", n)
	}
}

func TestUploadDocument_InvalidPriority(t *testing.T) {
	base, client, root := newDocumentsTestServer(t)

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("priority", "urgent"); err != nil {
		t.Fatalf("write field: %v", err)
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="file"; filename="a.txt"`)
	h.Set("Content-Type", "text/plain")
	part, err := w.CreatePart(h)
	if err != nil {
		t.Fatalf("CreatePart: %v", err)
	}
	if _, err := part.Write([]byte("hello")); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	resp := postUpload(t, client, base, &buf, w.FormDataContentType())
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	if code := decodeErrCode(t, resp); code != "validation_error" {
		t.Fatalf("error code = %q, want validation_error", code)
	}
	// Priority is rejected before the object is written.
	if n := countObjects(t, root); n != 0 {
		t.Fatalf("rejected upload left %d objects behind", n)
	}
}

func TestUploadDocument_UnsupportedContentTypeLeavesNoOrphan(t *testing.T) {
	base, client, root := newDocumentsTestServer(t)

	ct := "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	body, formCT := uploadBody(t, "file", "contrato.docx", &ct, []byte("PK\x03\x04not really a docx"))
	resp := postUpload(t, client, base, body, formCT)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415, got %d", resp.StatusCode)
	}
	if code := decodeErrCode(t, resp); code != "unsupported_content_type" {
		t.Fatalf("error code = %q, want unsupported_content_type", code)
	}
	if n := countObjects(t, root); n != 0 {
		t.Fatalf("rejected upload left %d objects behind", n)
	}
}

// The part declares octet-stream, so the handler must sniff the real type from
// the leading bytes and accept the PNG.
func TestUploadDocument_SniffsOctetStream(t *testing.T) {
	base, client, _ := newDocumentsTestServer(t)

	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	png = append(png, bytes.Repeat([]byte{0}, 64)...)

	ct := "application/octet-stream"
	body, formCT := uploadBody(t, "file", "scan.bin", &ct, png)
	resp := postUpload(t, client, base, body, formCT)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d body=%s", resp.StatusCode, string(b))
	}

	var created createDocumentResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Document.ContentType != "image/png" {
		t.Fatalf("sniffed content type = %q, want image/png", created.Document.ContentType)
	}

	req, _ := http.NewRequest(http.MethodDelete, base+"/api/v1/documents/"+created.Document.ID.String(), nil)
	if delResp, err := client.Do(req); err == nil {
		delResp.Body.Close()
	}
}

func TestUploadDocument_TooLarge(t *testing.T) {
	base, client, root := newDocumentsTestServer(t)

	// The server was built with MaxUploadBytes = 1 MiB and allows another MiB
	// of form overhead, so 4 MiB is unambiguously over the ceiling.
	ct := "text/plain"
	body, formCT := uploadBody(t, "file", "big.txt", &ct, bytes.Repeat([]byte("a"), 4<<20))
	resp := postUpload(t, client, base, body, formCT)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", resp.StatusCode)
	}
	if code := decodeErrCode(t, resp); code != "upload_too_large" {
		t.Fatalf("error code = %q, want upload_too_large", code)
	}
	if n := countObjects(t, root); n != 0 {
		t.Fatalf("oversized upload left %d objects behind", n)
	}
}

func TestGetReport_NotReady(t *testing.T) {
	base, client, _ := newDocumentsTestServer(t)

	ct := "text/plain"
	body, formCT := uploadBody(t, "file", "novo.txt", &ct, []byte("sem PII"))
	resp := postUpload(t, client, base, body, formCT)
	var created createDocumentResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()

	id := created.Document.ID.String()
	repResp, err := client.Get(base + "/api/v1/documents/" + id + "/report")
	if err != nil {
		t.Fatalf("GET report: %v", err)
	}
	defer repResp.Body.Close()

	if repResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", repResp.StatusCode)
	}
	if code := decodeErrCode(t, repResp); code != "report_not_ready" {
		t.Fatalf("error code = %q, want report_not_ready", code)
	}

	req, _ := http.NewRequest(http.MethodDelete, base+"/api/v1/documents/"+id, nil)
	if delResp, err := client.Do(req); err == nil {
		delResp.Body.Close()
	}
}

// C4: erasure is defined as idempotent. The second DELETE must be a clean 404,
// never a 500, and no bytes may survive the first one.
func TestDeleteDocument_IsIdempotent(t *testing.T) {
	base, client, root := newDocumentsTestServer(t)

	ct := "text/plain"
	body, formCT := uploadBody(t, "file", "apagar.txt", &ct, []byte("CPF: 529.982.247-25"))
	resp := postUpload(t, client, base, body, formCT)
	var created createDocumentResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()

	id := created.Document.ID.String()

	req, _ := http.NewRequest(http.MethodDelete, base+"/api/v1/documents/"+id, nil)
	first, err := client.Do(req)
	if err != nil {
		t.Fatalf("first DELETE: %v", err)
	}
	defer first.Body.Close()
	if first.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(first.Body)
		t.Fatalf("first DELETE: expected 200, got %d body=%s", first.StatusCode, string(b))
	}
	if n := countObjects(t, root); n != 0 {
		t.Fatalf("erasure left %d objects behind", n)
	}

	req2, _ := http.NewRequest(http.MethodDelete, base+"/api/v1/documents/"+id, nil)
	second, err := client.Do(req2)
	if err != nil {
		t.Fatalf("second DELETE: %v", err)
	}
	defer second.Body.Close()
	if second.StatusCode != http.StatusNotFound {
		t.Fatalf("second DELETE: expected 404, got %d", second.StatusCode)
	}
}

func TestDocumentRoutes_InvalidUUID(t *testing.T) {
	base, client, _ := newDocumentsTestServer(t)

	resp, err := client.Get(base + "/api/v1/documents/not-a-uuid")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

// With no object store configured every /documents route must degrade to 503
// rather than nil-panic. Needs no database.
func TestDocumentRoutes_DisabledWithoutObjectStore(t *testing.T) {
	srv := NewServer(Config{Port: "0"}, zap.NewNop(), nil, nil)
	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()

	id := uuid.New().String()
	cases := []struct{ method, path string }{
		{http.MethodPost, "/api/v1/documents"},
		{http.MethodGet, "/api/v1/documents/" + id + "/report"},
		{http.MethodDelete, "/api/v1/documents/" + id},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, ts.URL+tc.path, strings.NewReader(""))
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			if tc.method == http.MethodPost {
				req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
			}
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("expected 503, got %d", resp.StatusCode)
			}
			if code := decodeErrCode(t, resp); code != "documents_disabled" {
				t.Fatalf("error code = %q, want documents_disabled", code)
			}
		})
	}
}
