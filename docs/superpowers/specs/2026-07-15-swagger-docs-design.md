# Swagger docs for the public listings API

Date: 2026-07-15
Status: approved

## Goal

Interactive API documentation for the public read-only listings API, using the
same workflow as the CineplexMedia backend (swaggo/swag annotations, generated
spec committed to the repo, browsable Swagger UI).

## Decisions (confirmed with Marko)

- **Scope:** document only the public API — `GET /api/v1/properties` and
  `GET /api/v1/properties/{zpid}`. The Roku feed and `/healthz` stay
  undocumented.
- **Exposure:** the Swagger UI is served unconditionally, including on
  production (`https://api.dwellings.tv/swagger/index.html`). The API is
  public and read-only; docs are a feature.

## Approach

swaggo/swag annotation comments + `swaggo/http-swagger` to serve the UI
(the stdlib `net/http` equivalent of the gin-swagger setup in CineplexMedia).

Rejected alternatives:

- Hand-written `openapi.yaml` + embedded UI — no codegen dependency, but the
  spec drifts from the code and doesn't match the existing swag workflow.
- Spec-first framework (huma etc.) — would mean rewriting working handlers.

## Components

1. **General API annotations** in `cmd/server/main.go`: title "Dwellings API",
   version, host `api.dwellings.tv`, basePath `/`.
2. **Handler annotations** on `handleList` / `handleDetail` in
   `internal/api/api.go`: every query parameter accepted by
   `parseListParams` (`zip`, `city`, `state`, `property_type`, `min_price`,
   `max_price`, `min_beds`, `min_baths`, `min_sqft`, `max_sqft`, `sort`,
   `limit`, `cursor`), response schemas referencing the existing DTO structs
   (`listResponse`, `listItem`, `detailResponse`, `agentDTO`), and the
   400/404/500 error shape (`{"error": "..."}`).
3. **Generated `docs/` package** (`docs.go`, `swagger.json`, `swagger.yaml`)
   committed to the repo, next to `docs/superpowers/`.
4. **UI route** `GET /swagger/` registered in `internal/server/server.go` via
   `httpSwagger.WrapHandler`; blank-import of the generated docs package.
5. **Makefile target** `make swagger` that regenerates the spec with a pinned
   `go run github.com/swaggo/swag/cmd/swag@<version> init …` so no global
   install is required.

## Testing

- New smoke test in `internal/server`: `GET /swagger/index.html` and
  `GET /swagger/doc.json` return 200 (written first, TDD).
- Existing `internal/api` tests stay green.
- Local end-to-end check: run the server, curl the UI and `doc.json`.

## Error handling

Nothing new at runtime — the spec is static, embedded at build time. Drift is
handled by re-running `make swagger`.
