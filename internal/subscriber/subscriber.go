// Package subscriber records platform subscribers: viewers who hand over an
// email address (from the Roku app or the landing page) to hear about the
// platform. Same contract as the Cineplex backend's /api/v1/platform_subscriber
// so the Roku helper is portable.
package subscriber

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"
	"time"
)

// Subscriber is one platform_subscribers row.
type Subscriber struct {
	ID        int64           `json:"id"`
	Email     string          `json:"email"`
	Metadata  json.RawMessage `json:"metadata"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// Store persists subscribers.
type Store interface {
	// Upsert inserts the subscriber or, when the email is already known,
	// replaces its metadata.
	Upsert(ctx context.Context, email string, metadata json.RawMessage) error
}

// maxBodyBytes bounds the request body; metadata is a small JSON blob.
const maxBodyBytes = 64 << 10

// Handler serves the subscriber endpoint.
type Handler struct {
	store Store
	log   *slog.Logger
}

// NewHandler creates a Handler.
func NewHandler(store Store, log *slog.Logger) *Handler {
	return &Handler{store: store, log: log}
}

// Register mounts the routes on mux (server.Mounter).
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("PUT /api/v1/platform_subscriber", h.handleSubscribe)
	mux.HandleFunc("OPTIONS /api/v1/platform_subscriber", h.handlePreflight)
}

// subscribeRequest is the body of PUT /api/v1/platform_subscriber.
type subscribeRequest struct {
	// Email is required.
	Email string `json:"email" example:"viewer@example.com"`
	// Metadata is any JSON value describing the subscriber (device, source,
	// ...). Optional; stored as given and replaced on every call.
	Metadata json.RawMessage `json:"metadata,omitempty" swaggertype:"object"`
}

// subscribeResponse is the 200 body.
type subscribeResponse struct {
	Message string `json:"message" example:"Platform subscriber updated successfully"`
}

// validationResponse is the 400 body: one message per invalid field.
type validationResponse struct {
	Errors map[string]string `json:"errors"`
}

// errorResponse is the 500 body.
type errorResponse struct {
	Error string `json:"error"`
}

// handleSubscribe upserts a platform subscriber.
//
//	@Summary		Subscribe to the platform
//	@Description	Registers an email address as a platform subscriber. Calling it again for the same email replaces the stored metadata.
//	@Tags			subscribers
//	@Accept			json
//	@Produce		json
//	@Param			body	body		subscribeRequest	true	"Subscriber"
//	@Success		200		{object}	subscribeResponse
//	@Failure		400		{object}	validationResponse
//	@Failure		500		{object}	errorResponse
//	@Router			/api/v1/platform_subscriber [put]
func (h *Handler) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	cors(w)
	w.Header().Set("Cache-Control", "no-store")

	var req subscribeRequest
	body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{Error: "request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, validationResponse{Errors: map[string]string{"body": "The request body must be a JSON object."}})
		return
	}
	if errs := req.validate(); len(errs) > 0 {
		writeJSON(w, http.StatusBadRequest, validationResponse{Errors: errs})
		return
	}

	email := normalizeEmail(req.Email)
	if err := h.store.Upsert(r.Context(), email, req.Metadata); err != nil {
		h.log.Error("platform subscriber upsert failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "Failed to subscribe"})
		return
	}
	writeJSON(w, http.StatusOK, subscribeResponse{Message: "Platform subscriber updated successfully"})
}

// handlePreflight answers the CORS preflight a browser sends before a
// cross-origin PUT with a JSON body (the landing page).
func (h *Handler) handlePreflight(w http.ResponseWriter, _ *http.Request) {
	cors(w)
	w.Header().Set("Access-Control-Max-Age", "86400")
	w.WriteHeader(http.StatusNoContent)
}

func cors(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "PUT, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
}

// validate reports the invalid fields, keyed by their JSON name.
func (r *subscribeRequest) validate() map[string]string {
	errs := map[string]string{}
	switch email := strings.TrimSpace(r.Email); {
	case email == "":
		errs["email"] = "The email field is required."
	case !validEmail(email):
		errs["email"] = "The email field must be a valid email address."
	}
	// A JSON null is "no metadata", like an absent key.
	if m := strings.TrimSpace(string(r.Metadata)); m == "null" {
		r.Metadata = nil
	} else if len(r.Metadata) > 0 && !json.Valid(r.Metadata) {
		errs["metadata"] = "The metadata field must be valid JSON."
	}
	return errs
}

// validEmail accepts a bare address (no display name) with a dotted domain.
func validEmail(s string) bool {
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != s {
		return false
	}
	at := strings.LastIndexByte(s, '@')
	return at > 0 && strings.Contains(s[at+1:], ".")
}

// normalizeEmail is how the unique key is spelled: trimmed, lower-cased.
func normalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
