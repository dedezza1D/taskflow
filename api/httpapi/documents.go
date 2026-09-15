package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/dedezza1D/taskflow/internal/observability"
	"github.com/dedezza1D/taskflow/internal/pipeline"
	"github.com/dedezza1D/taskflow/internal/queue"
	"github.com/dedezza1D/taskflow/internal/store"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.uber.org/zap"
)

// publishTaskMessage enqueues a task message (reference payload only) with
// trace/request-id propagation. A publish failure is logged, not surfaced: the
// task row is durable and the reconciler recovers lost enqueues at every
// publish point — this is that contract, applied at the API publish point.
func (s *Server) publishTaskMessage(r *http.Request, task *store.Task) {
	if s.queue == nil {
		return
	}
	hdr := nats.Header{}
	if rid, ok := observability.RequestIDFromContext(r.Context()); ok && rid != "" {
		hdr.Set("X-Request-Id", rid)
	}
	otel.GetTextMapPropagator().Inject(r.Context(), observability.NATSHeaderCarrier{H: hdr})

	subject := queue.SubjectForPriority(string(task.Priority))
	err := s.queue.PublishTask(r.Context(), subject, queue.TaskMessage{
		TaskID:   task.ID.String(),
		Priority: string(task.Priority),
	}, hdr)
	if err != nil {
		s.logger.Warn("failed to enqueue task", zap.Error(err), zap.String("task_id", task.ID.String()))
	}
}

func (s *Server) documentsEnabled(w http.ResponseWriter) bool {
	if s.objects == nil {
		writeErr(w, http.StatusServiceUnavailable, "documents_disabled",
			"object storage is not configured (OBJECTS_DIR)")
		return false
	}
	return true
}

type createDocumentResponse struct {
	Document store.Document `json:"document"`
	TaskID   string         `json:"task_id"`
}

// handleCreateDocument accepts a multipart upload (field "file", optional field
// "priority"), stores the bytes in object storage, creates the documents row,
// and enqueues the document.process task with a REFERENCE payload — bytes never
// touch the task, the queue, or the DLQ (C1).
func (s *Server) handleCreateDocument(w http.ResponseWriter, r *http.Request) {
	if !s.documentsEnabled(w) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.maxUploadBytes+(1<<20)) // + form overhead
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		// Distinguish "too big" from "malformed": the client can act on the
		// first (send a smaller file) but not on the second.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "upload_too_large",
				fmt.Sprintf("upload exceeds the %d byte limit", s.maxUploadBytes))
			return
		}
		writeErr(w, http.StatusBadRequest, "invalid_multipart",
			fmt.Sprintf("expected multipart/form-data with a 'file' field (max %d bytes): %v", s.maxUploadBytes, err))
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	file, header, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", "'file' field is required")
		return
	}
	defer file.Close()

	priority := store.PriorityNormal
	switch r.FormValue("priority") {
	case "", "normal":
	case "low":
		priority = store.PriorityLow
	case "high":
		priority = store.PriorityHigh
	default:
		writeErr(w, http.StatusBadRequest, "validation_error", "priority must be low|normal|high")
		return
	}

	// Content type: trust an explicit part header, otherwise sniff.
	head := make([]byte, 512)
	n, err := io.ReadFull(file, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "read_error", err.Error())
		return
	}
	head = head[:n]

	contentType := strings.ToLower(strings.TrimSpace(strings.Split(header.Header.Get("Content-Type"), ";")[0]))
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = strings.ToLower(strings.Split(http.DetectContentType(head), ";")[0])
	}

	if !isSupportedContentType(contentType) {
		writeErr(w, http.StatusUnsupportedMediaType, "unsupported_content_type",
			fmt.Sprintf("%q is not supported; supported: %s",
				contentType, strings.Join(pipeline.SupportedContentTypes(), ", ")))
		return
	}

	docID := uuid.New()
	key := "documents/" + docID.String() + "/original"
	uri, err := s.objects.Put(r.Context(), key, io.MultiReader(strings.NewReader(string(head)), file))
	if err != nil {
		s.logger.Error("object store put failed", zap.Error(err))
		writeErr(w, http.StatusInternalServerError, "internal_error", "failed to store document")
		return
	}

	doc, err := s.store.CreateDocumentWithID(r.Context(), docID, store.CreateDocumentParams{
		Filename:    header.Filename,
		ContentType: contentType,
		StorageURI:  uri,
		OrgID:       principal(r).OrgID,
	})
	if err != nil {
		// Best-effort cleanup: never leave orphaned bytes without a row to
		// enumerate them from (erasure depends on enumerability).
		_ = s.objects.RemovePrefix(r.Context(), "documents/"+docID.String())
		s.logger.Error("create document failed", zap.Error(err))
		writeErr(w, http.StatusInternalServerError, "internal_error", "failed to create document")
		return
	}

	payload, err := json.Marshal(pipeline.Payload{
		DocumentID:  doc.ID.String(),
		StorageURI:  uri,
		ContentType: contentType,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", "failed to build task payload")
		return
	}

	task, err := s.store.CreateTask(r.Context(), store.CreateTaskParams{
		Type:     pipeline.TaskType,
		Payload:  payload,
		Priority: priority,
		OrgID:    principal(r).OrgID,
	})
	if err != nil {
		s.logger.Error("create document task failed", zap.Error(err))
		writeErr(w, http.StatusInternalServerError, "internal_error", "failed to create processing task")
		return
	}
	if err := s.store.SetDocumentTask(r.Context(), doc.ID, task.ID); err != nil {
		s.logger.Warn("link document to task failed", zap.Error(err))
	}
	doc.TaskID = &task.ID

	s.publishTaskMessage(r, task)

	writeJSON(w, http.StatusCreated, createDocumentResponse{Document: *doc, TaskID: task.ID.String()})
}

func isSupportedContentType(ct string) bool {
	for _, s := range pipeline.SupportedContentTypes() {
		if s == ct {
			return true
		}
	}
	return false
}

type listDocumentsResponse struct {
	Items  []store.Document `json:"items"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
}

// handleListDocuments lists documents newest-first with the same query-param
// conventions as handleListTasks (?status=, ?limit=, ?offset=). Rows carry
// status + failed_stage only — never document content — so listing is safe to
// expose without touching the C-guarantees.
func (s *Server) handleListDocuments(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()

	var status *store.DocumentStatus
	if v := qp.Get("status"); v != "" {
		sv := store.DocumentStatus(v)
		switch sv {
		case store.DocUploaded, store.DocProcessing, store.DocCompleted, store.DocFailed, store.DocErased:
			status = &sv
		default:
			writeErr(w, http.StatusBadRequest, "validation_error", "invalid status")
			return
		}
	}

	limit := 50
	if v := qp.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeErr(w, http.StatusBadRequest, "validation_error", "limit must be 1..200")
			return
		}
		limit = n
	}

	offset := 0
	if v := qp.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "validation_error", "offset must be >= 0")
			return
		}
		offset = n
	}

	items, err := s.store.ListDocuments(r.Context(), store.ListDocumentsParams{
		Status: status,
		Limit:  limit,
		Offset: offset,
		OrgID:  principal(r).OrgID,
	})
	if err != nil {
		s.logger.Error("list documents failed", zap.Error(err))
		writeErr(w, http.StatusInternalServerError, "internal_error", "failed to list documents")
		return
	}

	writeJSON(w, http.StatusOK, listDocumentsResponse{Items: items, Limit: limit, Offset: offset})
}

type getDocumentResponse struct {
	Document  store.Document           `json:"document"`
	Artifacts []store.DocumentArtifact `json:"artifacts"`
}

func (s *Server) handleGetDocument(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDVar(w, r)
	if !ok {
		return
	}

	doc, err := s.store.GetDocumentForOrg(r.Context(), id, principal(r).OrgID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "document not found")
			return
		}
		s.logger.Error("get document failed", zap.Error(err))
		writeErr(w, http.StatusInternalServerError, "internal_error", "failed to load document")
		return
	}

	artifacts, err := s.store.ListArtifacts(r.Context(), id)
	if err != nil {
		s.logger.Error("list artifacts failed", zap.Error(err))
		writeErr(w, http.StatusInternalServerError, "internal_error", "failed to load artifacts")
		return
	}
	if artifacts == nil {
		artifacts = []store.DocumentArtifact{}
	}

	writeJSON(w, http.StatusOK, getDocumentResponse{Document: *doc, Artifacts: artifacts})
}

// handleGetDocumentReport streams the report artifact.
func (s *Server) handleGetDocumentReport(w http.ResponseWriter, r *http.Request) {
	if !s.documentsEnabled(w) {
		return
	}
	id, ok := parseIDVar(w, r)
	if !ok {
		return
	}

	doc, err := s.store.GetDocumentForOrg(r.Context(), id, principal(r).OrgID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "document not found")
			return
		}
		s.logger.Error("get document failed", zap.Error(err))
		writeErr(w, http.StatusInternalServerError, "internal_error", "failed to load document")
		return
	}

	art, err := s.store.GetArtifact(r.Context(), id, pipeline.StageReport, pipeline.KindReport)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "report_not_ready",
			fmt.Sprintf("report not generated yet (document status: %s)", doc.Status))
		return
	}
	if err != nil {
		s.logger.Error("get report artifact failed", zap.Error(err))
		writeErr(w, http.StatusInternalServerError, "internal_error", "failed to load report")
		return
	}

	rc, err := s.objects.Get(r.Context(), art.StorageURI)
	if err != nil {
		s.logger.Error("open report object failed", zap.Error(err))
		writeErr(w, http.StatusInternalServerError, "internal_error", "failed to open report")
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

type eraseDocumentResponse struct {
	DocumentID string `json:"document_id"`
	Status     string `json:"status"`
}

// handleDeleteDocument is C4 — right to erasure (GDPR Art. 17). The order is
// deliberate:
//
//  1. TOMBSTONE the row (status=erased, optimistic): from this instant any
//     worker holding an in-flight message re-reads a terminal status and acks
//     without touching the bytes.
//  2. Remove every object under documents/{id}/ — original and all stage
//     artifacts (the prefix is the enumerable byte-side inventory).
//  3. Delete the task row (executions cascade). A later redelivery of its
//     message hits the engine's existing "task not found → ack" rule; DLQ
//     entries need no purge because they hold references only (C1) and the
//     bytes those references point to are now gone.
//  4. Delete the documents row (artifact rows cascade).
//
// Every step is idempotent, so a crash mid-sequence is fixed by calling DELETE
// again.
func (s *Server) handleDeleteDocument(w http.ResponseWriter, r *http.Request) {
	if !s.documentsEnabled(w) {
		return
	}
	id, ok := parseIDVar(w, r)
	if !ok {
		return
	}

	doc, err := s.store.GetDocumentForOrg(r.Context(), id, principal(r).OrgID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "document not found")
			return
		}
		s.logger.Error("get document failed", zap.Error(err))
		writeErr(w, http.StatusInternalServerError, "internal_error", "failed to load document")
		return
	}

	// 1) tombstone
	tombstoned := false
	for i := 0; i < 5 && !tombstoned; i++ {
		_, err := s.store.UpdateDocumentStatus(r.Context(), id, doc.Version, store.DocErased, nil)
		switch {
		case err == nil:
			tombstoned = true
		case errors.Is(err, store.ErrVersionConflict):
			if doc, err = s.store.GetDocumentForOrg(r.Context(), id, principal(r).OrgID); err != nil {
				writeErr(w, http.StatusInternalServerError, "internal_error", "failed to tombstone document")
				return
			}
		default:
			s.logger.Error("tombstone failed", zap.Error(err))
			writeErr(w, http.StatusInternalServerError, "internal_error", "failed to tombstone document")
			return
		}
	}
	if !tombstoned {
		writeErr(w, http.StatusConflict, "conflict", "document is being updated; retry erasure")
		return
	}

	// 2) bytes
	if err := s.objects.RemovePrefix(r.Context(), "documents/"+id.String()); err != nil {
		s.logger.Error("erasure: object removal failed", zap.Error(err), zap.String("document_id", id.String()))
		writeErr(w, http.StatusInternalServerError, "internal_error",
			"failed to remove stored objects; document is tombstoned — retry erasure")
		return
	}

	// 3) task + executions
	if doc.TaskID != nil {
		if _, err := s.store.DeleteTask(r.Context(), *doc.TaskID); err != nil {
			s.logger.Error("erasure: task deletion failed", zap.Error(err), zap.String("document_id", id.String()))
			writeErr(w, http.StatusInternalServerError, "internal_error",
				"failed to remove task records; document is tombstoned — retry erasure")
			return
		}
	}

	// 4) document row (+ artifact rows via cascade)
	if _, err := s.store.DeleteDocument(r.Context(), id); err != nil {
		s.logger.Error("erasure: document deletion failed", zap.Error(err), zap.String("document_id", id.String()))
		writeErr(w, http.StatusInternalServerError, "internal_error",
			"failed to remove document record; retry erasure")
		return
	}

	s.logger.Info("document erased", zap.String("document_id", id.String()))
	writeJSON(w, http.StatusOK, eraseDocumentResponse{DocumentID: id.String(), Status: "erased"})
}

func parseIDVar(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(mux.Vars(r)["id"])
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", "invalid document id")
		return uuid.Nil, false
	}
	return id, true
}
