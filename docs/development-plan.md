# Scout — Development Plan

> Status: draft, not yet started. Living document — update as decisions change.
> Scope: spans all three Scout apps, kept here in `scout-be` for now.

## Context

Scout helps job seekers apply to jobs across three apps:

| App | Repo | State today |
|---|---|---|
| Chrome extension (MV3) | `scout-ext` | **Does not exist** |
| Backend (Go + Postgres) | `scout-be` | One 25-line stdlib health handler, zero dependencies |
| Dashboard (React) | `scout-fe` | React Router v8 SPA (`ssr: false`) + TS. Client half of auth built, waiting on the API |

This is greenfield. Nothing to refactor; the cost is all in decisions, and this plan makes them.

**Note:** `scout-be/README.md` is aspirational — it documents `internal/server/`, graceful shutdown, `slog`, and `/api/v1/ping`, none of which exist, and commits to a "stdlib only, zero third-party deps" stance this plan overrides. Rewrite it in Phase 0, don't append to it.

### Locked decisions

- **Stack:** gin-gonic + pgx/v5 + sqlc + goose, Postgres
- **Auth:** Google OIDC only in v1, **Authorization Code Flow + PKCE with the backend as a confidential client**. Neither client ever receives a Google token; the code is exchanged server-side. Web is a full-redirect BFF terminating in an opaque session cookie. The extension uses `chrome.identity.launchWebAuthFlow` to obtain a code and posts it to the backend, which exchanges it and returns a bearer access JWT + rotating refresh token. Provider abstraction (`identities` table) so email/password can be added later without touching session handling.
- **Contract:** OpenAPI-first. `openapi.yaml` is truth → `oapi-codegen` (Go, `gin-server` generator) + `openapi-typescript` (TS clients).
- **Repos:** three separate. Generated TS client published as a private npm package.
- **PII:** transport + at-rest only. No app-level envelope encryption. *(One carve-out proposed — see Risk 4.)*
- **LLM:** not in v1. Schema and service seams built for it; deterministic rules-based scoring ships first.
- **Deploy:** production-shaped, cheaply hosted (Fly.io). Real CI, migrations, observability, backups. Not k8s.

---

## A. Backend layout

```
scout-be/
├── api/openapi.yaml              # source of truth
├── cmd/
│   ├── server/main.go
│   └── worker/main.go            # async job runner (own process from day one)
├── internal/
│   ├── config/                   # env → struct, validated at boot, exit(1) on missing
│   ├── telemetry/                # slog JSON handler + OTel setup
│   ├── httpapi/
│   │   ├── gen/                  # oapi-codegen gin-server output — COMMITTED
│   │   ├── middleware/           # gin.HandlerFunc chain
│   │   └── *.go                  # ServerInterface impl, thin: bind → service → encode
│   ├── auth/                     # OIDC code exchange; web sessions; JWT + refresh rotation (ext)
│   ├── store/
│   │   ├── gen/                  # sqlc output — COMMITTED
│   │   ├── queries/*.sql         # hand-written SQL
│   │   └── db.go                 # pgxpool, tx helper
│   ├── ingest/                   # extension event pipeline + projection
│   ├── jobs/                     # posting normalization, dedup, canonicalization
│   ├── applications/             # lifecycle + stage transitions
│   ├── profiles/                 # autofill profile, work history, education
│   ├── documents/                # resume metadata + presigned blob URLs
│   ├── matching/                 # Scorer interface; rules_v1 impl
│   ├── analytics/                # dashboard aggregates
│   ├── blob/                     # R2/S3 client
│   └── worker/                   # SKIP LOCKED dequeue loop
├── migrations/                   # goose .sql, embedded via embed.FS
├── deploy/                       # fly.toml, compose
├── Dockerfile · docker-compose.yml · Makefile · .golangci.yml
```

**Layering:** handler → service package → store. Three layers, no more. Handlers hold zero business logic; service packages own domain types and rules; `store/gen` is the only thing that touches SQL. Define interfaces at the *consumer*, not alongside the implementation — no `interface{ ... }` mirror of every struct.

**Generated code is committed.** CI runs `make generate` and fails if the tree is dirty. That single check catches the entire class of "forgot to regenerate" bugs.

---

## B. Postgres schema

### Conventions

- **UUIDv7 primary keys.** IDs are exposed to two untrusted clients; sequential bigints leak volume and invite enumeration. UUIDv7 is time-sortable, so unlike v4 it keeps B-tree insert locality. Generate app-side (`uuid.NewV7()`) so the ingest idempotency key can be client-minted.
- `timestamptz` everywhere, never `timestamp`. UTC. `created_at`/`updated_at NOT NULL DEFAULT now()`, `updated_at` maintained by trigger.
- **Hard delete by default.** Blanket soft-delete poisons every query with `AND deleted_at IS NULL` and silently breaks tenant isolation the first time someone forgets it. Only `users.deleted_at` exists, to implement an account-deletion grace period; everything else cascades for real.

### Tenancy

Every user-scoped query takes `user_id` as a mandatory parameter with an explicit `WHERE user_id = $1`. Backed by a CI check that greps `store/queries/*.sql` and fails any query touching a user-scoped table without a `user_id` predicate — cheap, and it catches the one bug that actually matters.

Postgres **RLS is deferred to Phase 6** as defense-in-depth. It's the stronger guarantee, but it requires `SET LOCAL app.current_user_id` inside a transaction for every read, which taxes every code path. Query-layer enforcement plus the lint gets ~90% of the value at ~10% of the cost; RLS goes on top once the query surface has stopped moving.

### Identity

```sql
users(id uuid PK, email citext UNIQUE NOT NULL, display_name text,
      avatar_url text, created_at, updated_at, deleted_at)

identities(id uuid PK, user_id uuid FK→users ON DELETE CASCADE,
           provider text NOT NULL,          -- 'google'
           subject text NOT NULL,           -- Google 'sub' claim
           email_at_provider citext, created_at, updated_at,
           UNIQUE(provider, subject))
```

Key on `sub`, **never on email** — Google account emails change, `sub` doesn't. Keying on email is an account-takeover vector.

Both clients run the same Authorization Code flow against the same `identities` rows. They differ only in what the backend hands back at the end, so there are two session tables.

**Web — opaque server-side session.** No JWT: a JWT cannot be revoked before it expires, which makes sign-out and "log out everywhere" impossible to honour. A row lookup per request is cheap at this scale.

```sql
sessions(id uuid PK,
         token_hash bytea NOT NULL UNIQUE,  -- SHA-256 of 32 random bytes; never store plaintext
         user_id uuid FK→users ON DELETE CASCADE NOT NULL,
         expires_at timestamptz NOT NULL,   -- absolute cap, 30d
         last_seen_at timestamptz NOT NULL, -- sliding idle window, 7d
         revoked_at timestamptz,
         user_agent text, ip inet, created_at)
  INDEX(user_id) WHERE revoked_at IS NULL
```

The transient OIDC state — `state`, `nonce`, PKCE verifier and `return_to` — lives in a short-lived (~10 min) `HttpOnly` cookie written at `/auth/login` and consumed at `/auth/callback`. No table needed; it is single-use and self-expiring.

**Extension — bearer access JWT + rotating refresh token.** MV3 makes cookies from a `chrome-extension://` origin awkward, so the extension gets tokens instead.

```sql
refresh_tokens(id uuid PK, user_id uuid FK CASCADE,
               token_hash bytea NOT NULL UNIQUE,  -- SHA-256; never store plaintext
               family_id uuid NOT NULL,
               parent_id uuid,
               client text NOT NULL,              -- 'extension' (web uses `sessions`)
               expires_at timestamptz NOT NULL,
               used_at timestamptz, revoked_at timestamptz,
               user_agent text, ip inet, created_at)
```

Rotation with **reuse detection**: presenting a token whose `used_at IS NOT NULL` means it leaked → revoke the entire `family_id`. This is the piece most JWT implementations skip and it's the one that turns a stolen refresh token from permanent access into a single-use window.

### Jobs — canonical vs. per-user sighting

Two users independently scrape the same posting. Split it:

```sql
companies(id uuid PK, name text NOT NULL, normalized_name text NOT NULL,
          domain text, linkedin_slug text, logo_url text, created_at, updated_at)
  UNIQUE(domain) WHERE domain IS NOT NULL
  INDEX(normalized_name)

-- GLOBAL, not user-scoped
jobs(id uuid PK, company_id uuid FK,
     ats_type text,        -- greenhouse|lever|ashby|workday|linkedin|indeed|unknown
     external_id text,     -- stable ATS id when extractable from URL
     canonical_url text, title text NOT NULL, normalized_title text,
     location_raw text, location_city text, location_region text, location_country text,
     remote_type text, employment_type text,
     salary_min numeric, salary_max numeric, salary_currency text, salary_period text,
     description_text text, description_hash bytea,
     dedup_hash bytea GENERATED,   -- hash(company_id, normalized_title, location_city)
     posted_at, first_seen_at, last_seen_at, created_at, updated_at)
  UNIQUE(ats_type, external_id) WHERE external_id IS NOT NULL
  UNIQUE(dedup_hash)
  GIN(to_tsvector('english', description_text))

-- per-user observation, preserves provenance
job_sightings(id uuid PK, job_id uuid FK, user_id uuid FK CASCADE,
              source_url text NOT NULL, raw_payload jsonb NOT NULL,
              scraper_version text, seen_at, created_at)
  INDEX(user_id, seen_at DESC), INDEX(job_id)
```

Dedup ladder: `(ats_type, external_id)` when the URL exposes a stable ID (Greenhouse and Lever both do), falling back to `dedup_hash`. **Never write user-derived free text into `jobs`** — all raw scrape output stays in `job_sightings.raw_payload`. That boundary is what keeps a shared table from becoming a cross-tenant leak.

### Applications — current state + event log

```sql
application_stages(code text PK, rank smallint NOT NULL, label text NOT NULL,
                   is_terminal boolean NOT NULL, is_positive boolean)
-- saved, applied, screening, interview, offer, accepted, rejected, withdrawn, ghosted

applications(id uuid PK, user_id uuid FK CASCADE NOT NULL, job_id uuid FK NOT NULL,
             stage text FK→application_stages(code) NOT NULL DEFAULT 'saved',
             resume_id uuid FK→documents, cover_letter_id uuid FK→documents,
             source text,                  -- extension|manual|import
             applied_at timestamptz,
             first_response_at timestamptz,  -- denormalized for percentile queries
             last_activity_at, closed_at, outcome text, notes text,
             created_at, updated_at)
  UNIQUE(user_id, job_id)
  INDEX(user_id, applied_at DESC) INCLUDE (stage, job_id)
  INDEX(user_id, stage)

application_events(id uuid PK, application_id uuid FK CASCADE NOT NULL,
                   user_id uuid NOT NULL,   -- denormalized: tenant queries without a join
                   type text NOT NULL,      -- stage_changed|email_received|note_added|...
                   from_stage text, to_stage text,
                   actor text NOT NULL,     -- user|extension|system
                   occurred_at timestamptz NOT NULL, metadata jsonb, created_at)
  INDEX(application_id, occurred_at), INDEX(user_id, occurred_at DESC)
```

**Why both a `stage` column and an event log:** pure event sourcing makes every dashboard query a fold, and the dashboard *is* the product. A pure enum column loses the timeline that time-to-response metrics need. Denormalized current state + append-only log gives cheap `WHERE stage = 'interview'` and full history.

**Why a lookup table over a Postgres enum:** enum values can't be reordered or removed, and `ALTER TYPE` in migrations is awkward. The lookup table also carries `rank` (funnel ordering), `label` (UI text), and `is_terminal` — data the UI needs anyway.

`UNIQUE(user_id, job_id)` means reapplying to the same posting reuses the row. That's almost always the intent; the event log preserves the retry.

### Profile & documents

```sql
profiles(user_id uuid PK FK CASCADE,
         legal_first_name, legal_last_name, preferred_name,
         email citext, phone,
         address_line1, address_line2, city, region, postal_code, country,
         linkedin_url, github_url, portfolio_url,
         work_authorization text, requires_sponsorship boolean,
         willing_to_relocate boolean,
         desired_salary_min numeric, desired_salary_currency text,
         created_at, updated_at)

-- separate table: different sensitivity, different retention, different access path
profile_eeo(user_id uuid PK FK CASCADE,
            gender, race_ethnicity, veteran_status, disability_status,
            created_at, updated_at)

work_history(id, user_id FK CASCADE, company_name, title, location,
             start_date, end_date, is_current, description, sort_order, ...)
education(id, user_id FK CASCADE, institution, degree, field_of_study,
          start_date, end_date, sort_order, ...)

-- learns answers to recurring ATS screening questions
profile_answers(id uuid PK, user_id uuid FK CASCADE,
                question_hash bytea, question_text text,
                answer_text text, answer_type text,
                use_count int, last_used_at,
                UNIQUE(user_id, question_hash))

documents(id uuid PK, user_id uuid FK CASCADE,
          kind text,                 -- resume|cover_letter|portfolio
          label, filename, content_type, size_bytes bigint,
          storage_key text NOT NULL, checksum_sha256 bytea,
          parsed_text text, parse_status text,
          is_default boolean, version int, created_at, updated_at)
  INDEX(user_id, kind)
  UNIQUE(user_id, kind) WHERE is_default
```

### Matching — the LLM seam

```sql
skills(id uuid PK, name text UNIQUE, normalized_name text UNIQUE, category text)
job_skills(job_id, skill_id, weight real, extracted_by text, PK(job_id, skill_id))
document_skills(document_id, skill_id, weight real, PK(document_id, skill_id))

match_scores(id uuid PK, user_id uuid FK CASCADE, job_id uuid FK, document_id uuid FK,
             score real NOT NULL,
             algorithm text NOT NULL,        -- 'rules_v1' → later 'llm_v1'
             algorithm_version text,
             breakdown jsonb,                -- matched/missing keywords, subscores
             computed_at,
             UNIQUE(user_id, job_id, document_id, algorithm, algorithm_version))
```

`algorithm` + `algorithm_version` in the unique key means an LLM scorer coexists with the rules scorer, scores are comparable across versions, and nothing needs backfilling when the algorithm changes.

### Ingestion & async work

```sql
ingest_events(id uuid PK,              -- CLIENT-generated UUIDv7 = the idempotency key
              user_id uuid FK CASCADE NOT NULL,
              type text NOT NULL, schema_version int NOT NULL,
              payload jsonb NOT NULL,
              client_ts timestamptz NOT NULL, received_at timestamptz DEFAULT now(),
              processed_at timestamptz, process_attempts int DEFAULT 0,
              process_error text, extension_version text)
  INDEX(user_id, received_at DESC)
  INDEX(processed_at) WHERE processed_at IS NULL

async_jobs(id uuid PK, user_id uuid, kind text NOT NULL, payload jsonb NOT NULL,
           status text NOT NULL,       -- pending|running|succeeded|failed|dead
           priority smallint DEFAULT 0,
           attempts int DEFAULT 0, max_attempts int DEFAULT 5,
           run_after timestamptz DEFAULT now(),
           locked_at, locked_by text, last_error text, result jsonb,
           created_at, updated_at)
  INDEX(status, run_after) WHERE status = 'pending'
```

Ingestion writes raw and returns immediately; a worker projects events into `jobs`/`applications`/`application_events`. Lossless, replayable when projection logic has bugs, and the natural place the LLM worker later attaches.

Dequeue with `SELECT … FOR UPDATE SKIP LOCKED` and `LISTEN/NOTIFY` to wake the worker. No Redis, no external queue — Postgres is more than sufficient at this scale, and one fewer moving part to operate.

Deferred to Phase 7: `prompt_versions(name, version, template, model, params)` and `llm_calls(user_id, async_job_id, prompt_version_id, tokens, cost_usd, latency_ms)` for prompt versioning and cost accounting.

---

## C. API surface

`E` = extension, `D` = dashboard.

**Auth — web (BFF)** `D` — `GET /auth/login?return_to=` (mint state + nonce + PKCE verifier, 302 → Google) · `GET /auth/callback` (verify state, exchange code, verify nonce, upsert identity, set session cookie, 302 → dashboard) · `GET /auth/me` · `POST /auth/logout` (revoke session)

**Auth — extension** `E` — `POST /auth/exchange` (code + PKCE verifier → token pair) · `POST /auth/refresh` (rotate) · `POST /auth/logout` (revoke family) · `GET /auth/me`

`GET /auth/me` is one handler serving both — the auth middleware has already resolved `user_id` from whichever credential the client presented. `return_to` **must** be validated against a same-origin absolute path before the callback redirects to it, or the endpoint is an open redirect on an auth path. `scout-fe`'s `safeRedirectPath` is the mirror of this check; the browser-side one guards nothing on its own.

**Ingest** — `POST /ingest/events` (batch, idempotent, per-event accept/duplicate result) `E` · `POST /jobs/resolve` (scraped posting → canonical job + existing application state + match score; the extension's "does this page matter" call) `E`

**Applications** — `GET /applications` (cursor, filters) `D` · `POST /applications` `E D` · `GET|PATCH|DELETE /applications/{id}` `D` · `POST /applications/{id}/stage` `D` · `GET /applications/{id}/events` `D`

**Jobs** — `GET /jobs/{id}` `E D` · `GET /jobs/{id}/match?document_id=` `E D`

**Profile** — `GET|PUT /profile` `E D` · `GET|PUT /profile/eeo` `E D` · CRUD `/profile/work-history`, `/profile/education` `D` · `GET|POST /profile/answers` `E D` · `GET /autofill/mapping?ats=greenhouse` (field map + values, single call) `E`

**Documents** — `GET /documents` `E D` · `POST /documents` → presigned PUT `D` · `POST /documents/{id}/finalize` `D` · `GET /documents/{id}/download` → presigned GET `E D` · `DELETE /documents/{id}` `D`

**Analytics** `D` — `/analytics/summary` · `/analytics/funnel` · `/analytics/timeseries?metric=&interval=week` · `/analytics/breakdown?dimension=company|source|ats|stage` · `/analytics/response-times`

**Account** — `POST /account/export` · `DELETE /account`

**Ops** — `/healthz` (liveness, no deps) · `/readyz` (DB ping) · `/metrics`

Blobs never proxy through Go. Presigned URLs only.

---

## D. Cross-cutting

**Middleware order** (outer → inner): RequestID → ClientIP → CORS → OTel → Logger → Recovery → Timeout → RateLimit → MaxBytesReader → Auth → route.

CORS sits near the outside so preflight `OPTIONS` short-circuits before auth and rate limiting.

| Concern | Implementation |
|---|---|
| RequestID | `gin-contrib/requestid` |
| ClientIP | not middleware — `engine.SetTrustedProxies(...)` + `c.ClientIP()` |
| CORS | `gin-contrib/cors`, exact-match allowlist from config |
| OTel | `otelgin` (`go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin`) |
| Logger | custom — `log/slog` JSON, request-scoped logger into `c.Set` |
| Recovery | custom — wraps `gin.Recovery` behaviour but emits `problem+json` |
| Timeout | `gin-contrib/timeout` |
| RateLimit | custom token bucket, in-memory to start |
| MaxBytesReader | custom — `c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, n)` |
| Auth | custom — session cookie (web) *or* bearer JWT (extension) → `user_id` in `c.Set` |

### gin specifics that bite

These are all consequences of gin using its own `*gin.Context` instead of the stdlib handler signature. None are blockers; all are silent-wrong-behaviour if missed.

- **`gin.New()`, never `gin.Default()`.** `Default()` installs gin's text-format Logger and a Recovery that writes plain text — both contradict the slog-JSON and RFC 9457 decisions below. Build the chain explicitly. Set `gin.SetMode(gin.ReleaseMode)` outside dev.
- **`SetTrustedProxies` is load-bearing for rate limiting.** Fly.io terminates TLS at its proxy, so without trusting it `c.ClientIP()` returns the proxy's address — and per-IP limits on the unauthenticated auth routes silently become one global bucket shared by every client. Trust Fly's proxy range explicitly; never `SetTrustedProxies(nil)` in production.
- **Override `NoRoute` and `NoMethod`.** Gin's defaults return plain-text 404/405, which breaks the one-error-shape contract both clients parse. `HandleMethodNotAllowed = true` must also be set explicitly or a wrong method returns 404 instead of 405.
- **Stdlib middleware needs `gin.WrapH` / `gin.WrapF`.** Anything written against `http.Handler` — including some OTel and OpenAPI validation helpers — does not compose directly.
- **Validation consolidates.** Gin's binding layer *is* `go-playground/validator/v10`, so `binding:"required,email"` tags on the generated request structs replace a separate validate step. One library, not two. Bind errors must still be translated into the `problem+json` `errors[]` array rather than returned raw.
- **OpenAPI request validation:** `github.com/oapi-codegen/gin-middleware` (`OapiRequestValidator`), mounted in dev/test only.

### CORS for a Chrome extension — the part that's genuinely unusual

- Extension origin is `chrome-extension://<32-char-id>`. Pin the ID by setting `key` in `manifest.json` so it's stable across builds and environments.
- Exact-match allowlist from config: `CORS_ALLOWED_ORIGINS=chrome-extension://<id>,https://app.scout.dev`. Never wildcard, never "any chrome-extension origin."
- **All backend calls must originate from the extension's service worker, not content scripts.** A content script's `fetch` carries the *page's* origin (`linkedin.com`), which you must never allowlist. Content script → `chrome.runtime.sendMessage` → service worker → API. This is a hard architectural constraint on the extension, not a preference.
- Add the API domain to `host_permissions` in the manifest.

### Credential placement differs per client — and the backend must serve both

| | What the client holds | Where |
|---|---|---|
| Dashboard | **Nothing.** No token ever reaches JavaScript. | `HttpOnly; Secure; SameSite=Lax; Domain=.scout.dev` session cookie |
| Extension | Access JWT | `chrome.storage.session` (memory-only, cleared on browser close) |
| Extension | Refresh token | `chrome.storage.local` |

The dashboard is the strong case: because the session is an opaque ID in an `HttpOnly` cookie, XSS on the dashboard cannot exfiltrate a durable credential. It buys this at the cost of a `GET /auth/me` round trip per page load, memoised client-side.

`SameSite=Lax` blocks cross-site POSTs, which covers most of CSRF for the dashboard; add a double-submit token for depth on mutating routes. The extension uses bearer headers, so it has no cookie and no CSRF surface at all.

**Deployment topology is load-bearing here.** The dashboard and API must sit on subdomains of one registrable domain (`app.scout.dev` / `api.scout.dev`) so the session cookie is first-party under `SameSite=Lax`. Splitting them across unrelated domains forces `SameSite=None`, which browsers are actively restricting.

### Everything else

- **Errors:** RFC 9457 `application/problem+json` — `{type, title, status, detail, instance, errors[], request_id}`. One shape, machine-readable, both clients parse it once.
- **Pagination:** keyset/cursor, base64 `(sort_key, id)`. Offset pagination duplicates and skips rows under concurrent inserts — exactly what an event-ingesting app does constantly.
- **Idempotency:** client-minted UUIDv7 per event + `INSERT … ON CONFLICT (id) DO NOTHING`. Response reports per-event status so the extension can clear its outbox precisely. `Idempotency-Key` header on other mutating endpoints.
- **Rate limiting:** per-IP on unauthenticated routes, per-user on authenticated. Token bucket, in-memory to start, Redis when you outgrow one instance. Return `429` + `Retry-After` + `RateLimit-*`; extension honors it with exponential backoff and jitter.
- **Validation:** gin's binding layer (`go-playground/validator/v10` under the hood) via `binding:"..."` tags, plus `oapi-codegen/gin-middleware` request validation in dev/test.
- **Config:** env → struct, validated at boot, non-zero exit on anything missing. No `.env` in production — Fly secrets.
- **Logging:** `log/slog` JSON, request-scoped logger carrying `request_id` and `trace_id`. **Because PII is unencrypted at rest, logs are the primary leak vector** — implement `slog.LogValuer` on every PII-bearing struct to force redaction, and add a CI grep for raw `%+v` on those types.
- **Server timeouts:** `ReadHeaderTimeout` (Slowloris — `gosec` flags its absence), `ReadTimeout`, `WriteTimeout`, `IdleTimeout`. Graceful shutdown on SIGINT/SIGTERM with a ~15s drain, then close pool and flush OTel.

---

## E. Analytics

Expected scale: thousands of applications per user, low thousands of users → low millions of rows.

**Compute on the fly. No materialized views, no rollup tables.** Postgres does this trivially at this size, and a rollup you don't need is a cache invalidation bug you didn't have to write. Revisit only when a query exceeds ~200ms against realistic volume — measured with `EXPLAIN (ANALYZE, BUFFERS)`, not guessed.

Queries needed: stage funnel with conversion rates (`count(*) FILTER (WHERE …)`), response rate (% reaching ≥ screening), time-to-first-response percentiles (`percentile_cont`), breakdowns by company / ATS / source / remote type, week-over-week trend, application velocity.

Two details that matter: `date_trunc` + `generate_series` to fill gap weeks (otherwise charts lie by omission), and the covering index `applications(user_id, applied_at DESC) INCLUDE (stage, job_id)` to keep funnel queries index-only.

---

## F. Testing

- **Unit** — pure logic only: match scoring, keyword extraction, title/company normalization, stage transition rules, JWT claims. No I/O.
- **Integration** — anything touching Postgres, via `testcontainers-go` with a real PG and real goose migrations. **Do not mock the database.** The entire value of sqlc is that the SQL is real; mocking it tests a fiction.
- **Handlers** — `httptest` against the real gin engine (`gin.SetMode(gin.TestMode)`) and real DB, with an injectable credential issuer so a test can mint either a session cookie or a bearer JWT. Table-driven, and every authenticated route exercised both ways — a route that only works for one client type is the failure mode dual-path auth invites. Build the engine through the same constructor production uses, so middleware-order regressions surface in tests.
- **Contract** — validate examples against `openapi.yaml` in CI via `kin-openapi`; plus the codegen-drift check (`make generate && git diff --exit-code`).
- **Fakes** — a local OIDC provider for Google: discovery document, authorization endpoint, **token endpoint** and JWKS. A JWKS server alone is not enough once the backend exchanges codes rather than verifying a client-supplied ID token. Plus minio for S3, and later the LLM provider. Real Postgres, fake everything external.
- **Analytics** — golden-file assertions against a seeded fixture dataset, plus a generator that seeds 10k applications for benchmarking.

---

## G. Phases

Each phase is a vertical slice that leaves the product working.

### Phase 0 — Foundation *(backend only)*
Rewrite README. Makefile, multi-stage Dockerfile (distroless, non-root), docker-compose, `.golangci.yml` with `gosec`/`errcheck`/`sqlclosecheck`. `config` + `telemetry` packages. gin router with the full middleware stack, incl. `NoRoute`/`NoMethod` and trusted-proxy config. pgxpool. goose with embedded migrations. sqlc config. CI: lint, test, `govulncheck`, codegen-drift.
**Exit:** `docker compose up` serves the API; `/healthz` + `/readyz` green; CI green; deployed to Fly with TLS on a real domain.

### Phase 1 — Identity
`users` / `identities` / `sessions` / `refresh_tokens`. go-oidc discovery + code exchange (`golang.org/x/oauth2/google`), `state` / `nonce` / PKCE handling, opaque session issue + lookup, JWT issue/verify, rotation with reuse detection, dual-path auth middleware, `return_to` validation, CORS allowlist with pinned extension ID, auth rate limits. **Establish the OpenAPI → codegen pipeline here** — this phase exists partly to prove that workflow before the surface grows.
Frontend work lands here too: scaffold `scout-ext` (MV3 + Vite + React). `scout-fe`'s client half is already built against these endpoints — it needs no change if the contract holds.
**Exit:** Google sign-in works from both dashboard and a stub extension; a tampered `state` and a replayed `nonce` are both rejected; refresh rotates; reusing a rotated token revokes the family; `GET /auth/me` returns from both origins; sign-out invalidates the session server-side.

### Phase 2 — Passive tracking *(capability 1)* ← the slice that makes it a product
`companies` / `jobs` / `job_sightings` / `applications` / `application_events` / `application_stages` / `ingest_events`. Batch idempotent `POST /ingest/events`, `POST /jobs/resolve`, projection worker on SKIP LOCKED, dedup + normalization.
Extension: content scripts for **Greenhouse and Lever only** — structured, stable, and not hostile. LinkedIn is deferred deliberately (Risk 1). Durable outbox in `chrome.storage.local` with `chrome.alarms` retry flush.
Dashboard: applications list.
**Exit:** applying on Greenhouse or Lever creates an application row visible in the dashboard within seconds.

### Phase 3 — Dashboard analytics
`analytics` package + aggregate queries, fixture/benchmark seeder, charts, and manual application entry so the dashboard stands alone without the extension.
**Exit:** funnel, response rate, time-to-response percentiles, breakdowns and WoW trend all render; every query <200ms against 10k seeded applications.

### Phase 4 — Profile + documents *(capabilities 4, then 2)*
`profiles` / `profile_eeo` / `work_history` / `education` / `documents` / `profile_answers`. Presigned upload/download to R2, resume text extraction as an `async_job`, `GET /autofill/mapping`. Extension field-mapping engine. PII redaction hardening pass.
**Autofill never auto-submits.** Fill, then require an explicit user click. Legal exposure and UX both point the same way.
**Exit:** profile and resumes managed in the dashboard; extension fills a Greenhouse and a Lever form correctly; a previously answered screening question is reused.

### Phase 5 — Match scoring *(capability 3, rules-based)*
`skills` / `job_skills` / `document_skills` / `match_scores`. TF-IDF-style keyword extraction over a curated skill taxonomy seed. `matching.Scorer` interface with the `rules_v1` implementation. Extension overlay UI.
**Exit:** overlay shows fit score and missing keywords on a supported posting; score recomputes when the selected resume changes.

### Phase 6 — Hardening
Postgres RLS as defense-in-depth. Automated backups **with a tested restore** — an untested backup is not a backup. OTel exporter to a real backend, alerting on error rate and p99. Secret rotation. Account export + deletion. Chrome Web Store submission prep (Risk 2).
**Exit:** restore drill passes, alerts fire on induced failure, security checklist clean.

### Phase 7 — LLM *(deferred)*
`llm_v1` behind the existing `Scorer` interface. `prompt_versions` + `llm_calls`, per-user cost caps. No schema migration of existing scores required — that's what the seam bought.

**Phases 0–3 are the credible MVP.** 4–5 are the differentiators.

---

## H. Risks

1. **LinkedIn scraping and automated form-filling.** LinkedIn's user agreement prohibits automated scraping and they enforce it — *hiQ v. LinkedIn* cut both ways (CFAA narrowed by *Van Buren*, but breach-of-contract survived), and the practical risk is your users getting their accounts restricted. Mitigation: build Greenhouse/Lever/Ashby first, treat LinkedIn as read-only over what the user is already viewing, never auto-submit, never bulk-apply.
2. **Chrome Web Store review.** Broad `host_permissions` (`*://*/*`) triggers manual review and rejection. Use narrow per-ATS match patterns plus `optional_host_permissions` requested at runtime. MV3 bans remotely-hosted code — no `eval`, no CDN scripts, everything bundled. A privacy policy and data-use disclosures are mandatory since you handle PII. Budget weeks, not days, for first approval.
3. **MV3 service worker lifecycle.** The worker is terminated after ~30s idle. No in-memory queues, no `setTimeout` beyond that window, no assumption of continuity. Everything durable in `chrome.storage.local`, `chrome.alarms` (30s minimum period) for retry flush, full state rehydration on every wake. This is the single largest source of "events silently disappeared" bugs in MV3 extensions.
4. **PII with at-rest-only encryption.** A defensible call — but the consequence is that a leaked DB dump or backup is a complete PII breach *including EEO/demographic data, which is special-category under GDPR Art. 9*. Compensating controls are non-optional: PII never in logs (enforced, not intended), encrypted and access-audited backups, EEO isolated in its own table. **One thing worth reconsidering:** `profile_eeo` is written once and never queried or filtered, so envelope-encrypting just that table costs nothing operationally and removes the worst-case disclosure. Small carve-out, disproportionate benefit.
5. ~~**`scout-fe` is on deprecated tooling.**~~ **Closed.** Migrated off CRA to React Router v8 framework mode (`ssr: false`) on Vite + TypeScript. The dashboard's auth client — session lookup, sign-in link, route gate, `return_to` safety — is built and green, and it is the reason the auth design above is a BFF: with no runtime Node server in `scout-fe`, the Go API is the only place OIDC can terminate.
6. **Shared `jobs` table is a cross-tenant surface.** Canonical postings are global by design. The discipline that keeps this safe is absolute: user-derived free text lives in `job_sightings.raw_payload`, never promoted into `jobs`.
7. **Scope vs. solo capacity.** Three apps and four extension capabilities is a lot. The phase ordering exists to keep something shippable at every point — resist working the four capabilities in parallel.

---

## Verification

**Per phase, before it's done:**
```bash
make generate && git diff --exit-code   # codegen is current
make lint                               # golangci-lint incl. gosec
make test                               # unit + testcontainers integration
govulncheck ./...
docker compose up --build               # boots clean from scratch
curl -fsS localhost:8080/readyz         # DB reachable
```

**Migrations:** every migration must apply *and roll back* against a scratch DB — `goose up && goose down-to <prev> && goose up`. Verified in CI, not by hand.

**Contract:** CI regenerates the Go server interfaces and TS client from `openapi.yaml`; a dirty tree fails the build. All three apps typecheck against the same generated artifacts.

**End-to-end, Phase 2 onward:** load the unpacked extension in Chrome, apply to a real Greenhouse posting, confirm the application appears in the dashboard, then kill the network mid-submit and confirm the outbox retries and does not double-write (the idempotency path is the one worth deliberately breaking).

**Analytics, Phase 3 onward:** seed 10k applications, run `EXPLAIN (ANALYZE, BUFFERS)` on every aggregate query, assert index-only scans on the funnel path and <200ms wall clock.

**Auth, ongoing:** integration tests that (a) rotate a refresh token, replay the consumed one, and assert the whole family is revoked; (b) drive `/auth/login` → `/auth/callback` against a local fake OIDC provider and assert that a mismatched `state`, a mismatched `nonce`, and a replayed authorization code are each rejected; (c) assert `return_to=//evil.com` and `return_to=https://evil.com` both fall back to the default path.
