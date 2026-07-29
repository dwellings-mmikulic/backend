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

// MapEnsurer returns a property's static map URL, generating it on demand.
// url is empty when there is no map; pending means one is still being
// generated in the background. May be nil when maps are disabled.
type MapEnsurer interface {
	Ensure(ctx context.Context, p *property.Property) (url string, pending bool)
}

// API serves the public read-only listings endpoints.
type API struct {
	repo Repo
	maps MapEnsurer
	log  *slog.Logger
}

// New creates the public API. maps may be nil, which disables property maps.
func New(repo Repo, maps MapEnsurer, log *slog.Logger) *API {
	return &API{repo: repo, maps: maps, log: log}
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
		w.Header().Set("Content-Type", "application/json")
		h(w, r)
	})
}

// handleList serves the browse endpoint.
//
//	@Summary		List properties
//	@Description	Browse active listings with filters, sorting, and cursor pagination. Pass the returned next_cursor to fetch the following page.
//	@Tags			properties
//	@Produce		json
//	@Param			zip				query		string	false	"Filter by ZIP code"
//	@Param			city			query		string	false	"Filter by city"
//	@Param			state			query		string	false	"Filter by state code (e.g. FL)"
//	@Param			property_type	query		string	false	"Filter by property type"
//	@Param			min_price		query		integer	false	"Minimum price in USD"
//	@Param			max_price		query		integer	false	"Maximum price in USD"
//	@Param			min_beds		query		integer	false	"Minimum bedrooms"
//	@Param			min_baths		query		number	false	"Minimum bathrooms"
//	@Param			min_sqft		query		integer	false	"Minimum home size in sqft"
//	@Param			max_sqft		query		integer	false	"Maximum home size in sqft"
//	@Param			sort			query		string	false	"Sort order"	Enums(newest, price_asc, price_desc)	default(newest)
//	@Param			limit			query		integer	false	"Page size (1-100)"	default(24)
//	@Param			cursor			query		string	false	"Opaque pagination cursor from a previous response's next_cursor"
//	@Success		200				{object}	listResponse
//	@Failure		400				{object}	errorResponse
//	@Failure		500				{object}	errorResponse
//	@Router			/api/v1/properties [get]
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

// handleDetail serves the detail endpoint.
//
//	@Summary		Get property detail
//	@Description	Full detail-screen payload for one listing, addressed by its Zillow property ID.
//	@Description	map_image_url is generated on first view; it may be null on the very first request for a listing and populated shortly after.
//	@Tags			properties
//	@Produce		json
//	@Param			zpid	path		string	true	"Zillow property ID"
//	@Success		200		{object}	detailResponse
//	@Failure		404		{object}	errorResponse
//	@Failure		500		{object}	errorResponse
//	@Router			/api/v1/properties/{zpid} [get]
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
	resp := toDetailResponse(p)

	// Generate the map on demand. Ensure waits a short while; if it is still
	// working we return a null map and shorten the cache so the next viewer
	// picks up the finished one quickly.
	pending := false
	if a.maps != nil {
		if url, stillWorking := a.maps.Ensure(r.Context(), p); url != "" {
			resp.MapImageURL = &url
		} else {
			pending = stillWorking
		}
	}

	cacheControl := defaultCacheControl
	if pending {
		cacheControl = pendingCacheControl
	}
	writeJSONCache(w, http.StatusOK, resp, cacheControl)
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

const (
	defaultCacheControl = "public, max-age=300"
	// pendingCacheControl is used when a map is still generating, so the null
	// map is not cached for the full window.
	pendingCacheControl = "public, max-age=30"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	writeJSONCache(w, status, v, defaultCacheControl)
}

func writeJSONCache(w http.ResponseWriter, status int, v any, cacheControl string) {
	if status < 400 {
		w.Header().Set("Cache-Control", cacheControl)
	} else {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// errorResponse is the body of every non-2xx response.
type errorResponse struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}
