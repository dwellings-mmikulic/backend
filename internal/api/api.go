package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/dwellingtw/backend/internal/property"
)

// Repo is the data access the public API needs.
type Repo interface {
	List(ctx context.Context, f property.Filter) ([]property.Property, int, bool, error)
	GetByZPID(ctx context.Context, zpid string) (*property.Property, error)
}

// API serves the public read-only listings endpoints.
type API struct {
	repo Repo
	log  *slog.Logger
}

// New creates the public API.
func New(repo Repo, log *slog.Logger) *API {
	return &API{repo: repo, log: log}
}

// Register mounts the public routes on mux.
func (a *API) Register(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/properties", a.public(a.handleList))
	mux.Handle("GET /api/v1/properties/{zpid}", a.public(a.handleDetail))
}

// public applies the headers every public API response carries.
func (a *API) public(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.Header().Set("Content-Type", "application/json")
		h(w, r)
	})
}

func (a *API) handleList(w http.ResponseWriter, r *http.Request) {
	f, err := parseListParams(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	props, total, hasMore, err := a.repo.List(r.Context(), f)
	if err != nil {
		a.log.Error("list properties failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	resp := listResponse{Total: total, Results: make([]listItem, 0, len(props))}
	for i := range props {
		resp.Results = append(resp.Results, toListItem(&props[i]))
	}
	if hasMore && len(props) > 0 {
		tok := encodeCursor(cursorForLast(f.Sort, &props[len(props)-1]))
		resp.NextCursor = &tok
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *API) handleDetail(w http.ResponseWriter, r *http.Request) {
	p, err := a.repo.GetByZPID(r.Context(), r.PathValue("zpid"))
	if errors.Is(err, property.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		a.log.Error("get property failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, toDetailResponse(p))
}

// cursorForLast builds the next-page cursor from the last row of this page.
func cursorForLast(sort property.Sort, last *property.Property) cursor {
	c := cursor{Sort: string(sort), ID: last.ID}
	switch sort {
	case property.SortPriceAsc, property.SortPriceDesc:
		c.Price = last.SalePrice
	default: // newest
		t := last.CreatedAt
		c.CreatedAt = &t
	}
	return c
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
