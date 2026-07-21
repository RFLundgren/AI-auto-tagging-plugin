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
| Prerequisite: plugin-writable tag endpoints | `navidrome-experimental` (core fork) | ✅ Done — merged via [PR #16](https://github.com/RFLundgren/navidrome_experimental/pull/16), live in the `:develop` image |
| Open decision: shared vs. private AI tags | Design decision, this doc | ✅ Implemented Option C (private); A/B remain open if ever needed |
| The plugin itself | This repo | ✅ Working, tested end-to-end in production against Gemini |
| Anthropic / OpenAI adapters | This repo | ⚠️ Unit-tested only — not yet live-verified against real accounts |
| Auto-generated playlists from tags | [ai-mood-playlists](https://github.com/RFLundgren/ai-mood-playlists) (separate repo, private fork for now) | 🚧 In progress — see that repo's own `PLAN.md` |

See [README.md § Status](README.md#status) for the user-facing summary, and the **Known gaps** section near the
bottom of this doc for the specific things left to do.

---

## Prerequisite: navidrome-experimental changes

**✅ Done.** Merged via [PR #16](https://github.com/RFLundgren/navidrome_experimental/pull/16) and live in the
`:develop` image. Kept below as a record of what was built and why — this was core-fork work, not part of this
repo.

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

**Decided: Option C, implemented and live.** Got a working end-to-end plugin fastest; upgrading to A or B remains an
option later if the "everyone sees AI tags" outcome ends up mattering in practice, but there's no current plan to
change it.

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

**Phase 0 — prerequisite.** ✅ Done. `setUserTag`/`removeUserTag`/`getUserTags` merged into `navidrome-experimental`.

**Phase 1 — plugin skeleton.** ✅ Done. Manifest, scheduler wiring, `kvstore`-backed high-water mark, reading tracks
via `SubsonicAPIService.Call`. Unit-tested with the PDK's native-build mocks (no TinyGo/WASM runtime needed for
`go test`).

**Phase 2 — one working provider adapter.** ✅ Done, live-tested against **Gemini** in production (not Claude as
originally planned — Gemini is what got tested first in practice). Full pipeline confirmed working: scan → skip
already-tagged → batch → classify → write via `setUserTag.view`, under Option C.

**Phase 3 — remaining provider adapters.** ✅ Anthropic and OpenAI adapters implemented behind the same
`Classify()` interface, unit-tested. ⚠️ Neither has been live-verified against a real account yet — that's the
main concrete gap left in this repo.

**Phase 4 — visibility upgrade, if warranted.** Not started, no current plan to do it — see the Open decision
section above.

**Phase 5 — auto-generated playlists from tags.** 🚧 New, not originally in this plan. Being built as a separate
project — see [ai-mood-playlists](https://github.com/RFLundgren/ai-mood-playlists)'s own `PLAN.md` for details.
That repo is a fork of an existing audio-analysis-based mood-playlist plugin, being reworked to build its
playlists from this plugin's tags instead of its own audio analysis.

## Verification

- ✅ Unit tests cover the scan/skip/batch logic and all three provider adapters, running natively via `go test`
  (the PDK's mock stubs, no TinyGo/WASM runtime needed).
- ✅ Live-tested end-to-end in production against a real Navidrome instance and a real Gemini API key: manifest
  install, permission grants, scheduled scan, task-queue processing, provider classification, and tag write-back
  all confirmed working. Along the way this surfaced and fixed: a manifest validation error (`subsonicapi` needs
  `users` declared too), a too-aggressive default task-retry backoff against provider rate limits, and a wrong
  model ID string (fixed via config, no code change needed).
- Anthropic and OpenAI still need the same live-verification pass Gemini got — not expected to surface anything
  new given they share the same interface, but not yet actually done.
- Before enabling `allUsers: true` (Option A) or the shared-owner union (Option B) against a real multi-user
  library, verify against a throwaway library/test users first — a broadcast-write bug would apply incorrect tags
  to every user's namespace at once, which is annoying to bulk-undo. (Not relevant unless A/B ever get picked up.)

## Known gaps / remaining work

- ✅ **Fixed vocabulary for genre/mood** — done. The prompt now constrains `genre` and `mood` tags to a fixed,
  small list each (`genreVocabulary`/`moodVocabulary` in `providers.go`), with a post-parse filter dropping
  anything the model returns outside that list. `language` stays open-vocabulary on purpose. This exists so
  ai-mood-playlists' "one playlist per discovered tag value" doesn't fragment into near-duplicates
  (`chill`/`relaxed`/`mellow` for the same concept) — see that repo's `PLAN.md`.
- **Anthropic/OpenAI live verification** — implemented and unit-tested, not yet run against a real account.
- **`tagCategories` scope** — currently defaults to all three (`genre`, `mood`, `language`). Discussed dropping
  `language` since the two-column playlist/browsing use case in mind only needs genre + mood — not yet actually
  changed in the manifest default or tested; currently just a `tagCategories` config edit away, no code change
  needed.
- **Two-column "My Tags" display** — splitting the Songs list's single merged tag column into separate
  genre/mood columns (stripping the `genre:`/`mood:` prefix for display, keeping it in the stored data) was
  discussed as a good readability win. This is a `navidrome-experimental` UI change (`ui/src/song/SongList.jsx`),
  not something this repo controls — not started.
- **Auto-generated playlists** — the actual motivating reason for the two-column idea and a bigger piece of
  work; tracked entirely in [ai-mood-playlists](https://github.com/RFLundgren/ai-mood-playlists), not here.

## Open questions, not resolved

- Whether `sharedTagOwner` (Option B) is a real Navidrome user account the admin creates, or a synthetic
  ID that never logs in — affects whether it needs to be excluded from user-facing lists (e.g. sharing
  dialogs, "who's online"). Moot unless Option B gets picked up.
- License for this repo — not yet decided.
