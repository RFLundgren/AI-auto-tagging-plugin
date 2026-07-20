# AI Auto-Tagging Plugin — Design & Build Plan

## Context

Auto-classify tracks by genre, language, mood, etc. using an AI service (a paid API like Claude/OpenAI/Gemini,
since local-LLM hardware isn't something most self-hosters have), so the whole library becomes filterable by
AI-suggested tags instead of manually maintained playlists per genre/language.

Originally scoped in `navidrome-experimental`'s
[FEATURE_ROADMAP.md](https://github.com/RFLundgren/navidrome_experimental/blob/master/FEATURE_ROADMAP.md)
(source: [navidrome/navidrome discussion #3145](https://github.com/navidrome/navidrome/discussions/3145)) and
pulled out into its own project once it became clear the plugin itself doesn't belong inside the server's repo —
see [README.md § Relationship to navidrome-experimental](README.md#relationship-to-navidrome-experimental) for why.

## Status at a glance

| Piece | Where it lives | Status |
|---|---|---|
| Prerequisite: plugin-writable tag endpoints | `navidrome-experimental` (core fork) | 📋 Not started |
| Open decision: shared vs. private AI tags | Design decision, this doc | ❓ Undecided |
| The plugin itself | This repo | 📋 Not started |

Nothing here is blocked on anything outside this plan — the prerequisite is small, and the open decision just needs
someone to pick an option before Phase 2 implementation begins in earnest (Phase 1 is identical regardless of which
option wins, so it can start immediately).

---

## Prerequisite: navidrome-experimental changes

This is core-fork work, not part of this repo — tracked in `navidrome-experimental`'s own FEATURE_ROADMAP.md, and
must land there (or at least be in progress) before this plugin can actually write tags.

**Why it's needed:** the plugin needs a way to apply/remove tags on a track. `navidrome-experimental` already has a
private per-user tagging system (`media_file_tag`, shipped for
[discussion #4823](https://github.com/navidrome/navidrome/discussions/4823)) with a `MediaFileTagRepository`, but
no Subsonic-tier endpoint exposes it for a plugin to call — plugins can only reach Navidrome through
`SubsonicAPIService.Call(ctx, uri)` (`plugins/host/subsonicapi.go`), which proxies any *registered* Subsonic-tier
route, not a fixed whitelist. So the fix is registering new routes, not a new plugin-system capability.

**What to add**, mirroring the existing `skip`/`unskip` fork-specific endpoints exactly
(`server/subsonic/media_annotation.go:158-207`, registered in `server/subsonic/api.go`'s existing authenticated
route group):

- `setUserTag.view` — `id` (song) + `tag` params in, calls `MediaFileTagRepository.TagSong`, `newResponse()` out.
- `removeUserTag.view` — same shape, calls `UntagSong`.
- `getUserTags.view` — `id` param in, calls `TagsForSong`, returns the current tag list. Exists so the plugin can
  check what's already applied before re-tagging (idempotent runs — don't re-classify a track every scheduled run).

Zero new persistence code needed — those repository methods already scope by `loggedUser(ctx)` internally. This is
a small, low-risk change reusing an established pattern, not new plugin-system surface.

---

## Open decision: shared vs. private AI tags

**The real scoping question, not a technical blocker.** `media_file_tag` is deliberately private-per-user — the
whole point of #4823 was that two people sharing a library never see each other's tags. AI classification is a
library *fact*, though, and the original discussion's ask was "browse/filter the whole library by AI tags," shared
like `genre`/`mood` are today — not personal opinion like "workout." Writing AI tags into the private table as-is
means only the identity the plugin authenticates as sees them. Pick one before starting Phase 2 implementation:

### Option A — Broadcast writes (zero further backend changes)

Plugin calls `UsersService.GetUsers(ctx)` (`plugins/host/users.go`, `allUsers: true` grant returns every real
user), then loops and calls `setUserTag.view?u=<username>&...` once per user per song per tag using the
prerequisite endpoint as-is.

- **Pros:** no backend work beyond the prerequisite; tags land in each user's real private namespace; fully
  consistent with #4823's privacy model.
- **Cons:** O(users × tracks × tags) write volume; requires the admin to grant broad `allUsers: true`; a user added
  after a classification run doesn't retroactively get already-applied tags.

### Option B — Shared "system" identity + read-path union (small backend change, O(1) writes)

One well-known "AI tags" owner (config-designated user ID or dedicated service account) in `navidrome-experimental`;
union that owner's rows into every read path — `mediaFileUserTagFilter`
(`persistence/mediafile_repository.go`, `Eq{"t.user_id": userID}` → `Or{Eq{"t.user_id": userID}, Eq{"t.user_id":
sharedTagOwnerID}}`), `userTagCond` (`persistence/criteria_sql.go`, same union for smart-playlist criteria), and the
`selectMediaFile` group_concat subquery ("My Tags" column). Plugin writes under one identity only.

- **Pros:** one write per song per tag; new users see AI tags immediately; narrower `subsonicapi` permission grant
  (scoped to one account, not `allUsers`).
- **Cons:** real new backend surface in `navidrome-experimental` (3 read-path call sites + a config setting for the
  shared owner + admin UX for configuring it); blurs the "tags are private" story #4823 established — needs some UI
  signal (e.g. a differently-labeled "AI Tags" filter alongside "My Tag") so tags don't appear to come from nowhere.

### Option C — Fully private, no visibility feature (baseline/fallback)

Plugin only ever tags under whichever single account it authenticates as (typically the installing admin).

- **Pros:** simplest possible scope, ships fastest.
- **Cons:** doesn't deliver the original discussion's "whole library filterable by everyone" outcome by default.

**Recommendation, not yet decided:** start with Option C to get a working end-to-end plugin fastest, upgrade to A or
B once the classification quality/cost tradeoffs are validated in practice. But this is a real product call, not a
technical one — make it deliberately, don't default into it.

---

## Architecture

### Provider adapter pattern (confirmed feasible, fully contained in this plugin — no navidrome-experimental changes)

The plugin's `http` permission isn't provider-specific, so supporting Claude, OpenAI, and Gemini from day one is
straightforward:

```
Classify(tracks []TrackInfo) ([]TagSuggestion, error)
```

One implementation per provider (`AnthropicAdapter`, `OpenAIAdapter`, `GeminiAdapter`), each building that
provider's own request/response shape, selected at startup by a `provider` config field.

**Constraint:** `requiredHosts` is fixed in the manifest at package build time, not editable per-install — so the
manifest allowlists all candidate providers' API hosts upfront (`api.anthropic.com`, `api.openai.com`,
`generativelanguage.googleapis.com`), and the `provider` config field just picks which one actually gets called at
runtime. Small, fixed, auditable list either way.

### Plugin capabilities needed

| Capability / host service | Why |
|---|---|
| `http` (with `requiredHosts` for all candidate providers) | Call the AI provider's API |
| `library` or `subsonicapi` | Read track metadata to classify (`search3?query=&songCount=N&songOffset=M`) |
| `scheduler` | Re-run periodically on newly-added tracks |
| `taskqueue` (`TaskWorker` capability) | Batch-process the whole library in the background, with retries and a
concurrency cap suited to a rate-limited external API — not inline in the scheduler callback |
| `kvstore` | Track a high-water mark for incremental re-runs (only classify tracks added since the last run) |
| `users` | Only needed if Option A (broadcast writes) is chosen |

Structurally, this is a composite of two existing official example plugins (useful as starting references, not to
copy verbatim):
- `plugins/examples/nowplaying-py` in `navidrome-experimental` — for the scheduling half (`scheduler` permission,
  `ScheduleRecurring`, `nd_on_init`/`nd_scheduler_callback`).
- `plugins/examples/wikimedia` — for the external-API half (`http` permission with `requiredHosts` allowlisting,
  JSON response parsing).

The one pattern neither example demonstrates: routing each track's classification through `taskqueue`/`TaskWorker`
rather than inline in the scheduler callback, for persistence across restarts, retries, and a concurrency cap. See
`nd_task_execute` in Navidrome's plugin docs, and `plugins/host_taskqueue_test.go` in `navidrome-experimental` for
reference behavior.

### Data flow

1. `nd_scheduler_callback` fires (or plugin init, for the first run).
2. Page through the library via `search3?query=&songCount=N&songOffset=M` through `SubsonicAPIService.Call`,
   starting from the high-water mark stored in `kvstore`.
3. For each track not already classified (checked via the prerequisite's `getUserTags.view`), enqueue a
   classification task via the `taskqueue` host service — batched (see Cost below), not one track per call.
4. `nd_task_execute` picks up each batch, calls the configured provider's adapter, gets back tag suggestions.
5. Write suggested tags via `setUserTag.view` (per Option A/B/C above).
6. Advance the `kvstore` high-water mark once a page completes successfully.

---

## Plugin config

`manifest.json`'s `config` block (JSON Schema + JSONForms `uiSchema`, admin-set on install via Navidrome's web UI):

| Field | Type | Notes |
|---|---|---|
| `provider` | enum (`anthropic`/`openai`/`gemini`) | Picks which adapter runs |
| `apiKey` | string, `ui:widget: password` | The selected provider's API key |
| `model` | string, e.g. `claude-haiku-4-5`/`gpt-5-mini`/`gemini-flash` | Kept separate from `provider` since
cost/quality varies a lot within one provider's own tiers |
| `tagCategories` | array/enum | Which categories to suggest: genre, mood, language, or all |
| `batchSize` | integer | Tracks per API call — see Cost below for why this matters |
| `maxTracksPerRun` | integer | Cost control ceiling per scheduled run |
| `sharedTagOwner` | string (username), only if Option B chosen | Target service-account identity for shared writes |

## Cost

At Claude Haiku 4.5 pricing ($1 / $5 per 1M input/output tokens) with 50-track batches (batching "Artist – Title"
pairs into one request rather than one call per track cuts cost roughly 3x by amortizing the instruction-prompt
overhead), roughly **$0.14 per 1,000 tracks classified**. API cost is a rounding error next to the engineering
effort here — the visibility decision (Option A/B/C above) is the real scoping question, not cost.

---

## Build plan

**Phase 0 — prerequisite.** `setUserTag`/`removeUserTag`/`getUserTags` land in `navidrome-experimental`. Blocks
Phase 2 (can't write real tags without it), but Phase 1 below doesn't depend on it.

**Phase 1 — plugin skeleton.** Manifest, scheduler wiring, `kvstore`-backed high-water mark, reading tracks via
`SubsonicAPIService.Call`. Buildable and testable (via the Extism CLI, no live Navidrome server needed for basic
capability-function testing) independent of the prerequisite or the AI provider — this is pure plumbing.

**Phase 2 — one working provider adapter.** Pick one provider (Claude, given the cost data above is already worked
out for it) and get a real end-to-end classification working: read tracks → call API → get tags → write via
`setUserTag.view` (needs Phase 0 done). Ship Option C (fully private) first to validate the whole pipeline before
adding the Option A/B visibility complexity.

**Phase 3 — remaining provider adapters.** OpenAI, Gemini — same `Classify()` interface, new adapter
implementations only.

**Phase 4 — visibility upgrade, if warranted.** Move from Option C to A or B based on what Phase 2/3 usage
actually shows about classification quality and how much the "everyone sees AI tags" outcome matters in practice.

## Verification

- Phase 1 (scheduler/kvstore/read plumbing) is testable standalone with the Extism CLI (`extism call plugin.wasm
  nd_scheduler_callback --wasi`), no live server needed for the parts that don't call `setUserTag.view`.
- Phase 2 needs a real `navidrome-experimental` instance with Phase 0 merged, and a real (or sandboxed/mocked)
  provider API key — test against a small library first, not the full collection, given real API cost is involved.
- Before enabling `allUsers: true` (Option A) or the shared-owner union (Option B) against a real multi-user
  library, verify against a throwaway library/test users first — a broadcast-write bug would apply incorrect tags
  to every user's namespace at once, which is annoying to bulk-undo.

## Open questions, not resolved

- Visibility option (A/B/C) — see above.
- Whether `sharedTagOwner` (Option B) is a real Navidrome user account the admin creates, or a synthetic
  ID that never logs in — affects whether it needs to be excluded from user-facing lists (e.g. sharing
  dialogs, "who's online").
- License for this repo — not yet decided.
