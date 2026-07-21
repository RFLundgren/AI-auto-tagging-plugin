# AI Auto-Tagging Plugin for Navidrome

A [Navidrome](https://www.navidrome.org/) plugin that auto-classifies tracks (genre, mood, language) using an AI
provider (Anthropic, OpenAI, or Gemini), so a whole library becomes filterable by AI-suggested tags instead of
relying on manually maintained playlists per genre/language. A companion project,
[AI Mood Playlists](https://github.com/RFLundgren/ai-mood-playlists), builds and maintains actual playlists from
these tags automatically — one per discovered genre/mood value — if you want that on top of just the tags
themselves.

Built against [navidrome-experimental](https://github.com/RFLundgren/navidrome_experimental) (a personal fork of
Navidrome), using its `media_file_tag` user-tagging feature via three Subsonic endpoints this project added there
(`setUserTag.view`, `removeUserTag.view`, `getUserTags.view`). See [PLAN.md](PLAN.md) for the full design and build
plan.

## Status

**Working, tested end-to-end in production** against a real Navidrome instance. The prerequisite endpoints are
merged into `navidrome-experimental`. Live-tested against Gemini; the Anthropic and OpenAI adapters share the same
`Classify()` interface and are covered by unit tests, but haven't yet been live-verified against a real account.

Tags are written under a single identity (Option C in PLAN.md — private, simplest to reason about). The
shared/broadcast visibility options (A/B) remain a deliberate future decision, not yet implemented.

## How it works

1. On a configurable schedule, the plugin pages through the library via `search3`, picking up where the last run
   left off.
2. Each track is checked for existing tags first (`getUserTags.view`) — **a track is only ever classified once.**
   Once it has any tag, every future scan skips it. The only recurring cost is a cheap, local, free Navidrome API
   check per track, not a repeated AI call.
3. Untagged tracks are batched and handed to the configured AI provider, which returns suggested tags per category
   (genre/mood/language), prefixed to avoid collisions in the shared freeform tag namespace (e.g. `genre:rock`,
   `mood:energetic`, `language:english`). `genre` and `mood` are constrained to a fixed, curated vocabulary (25
   genres, 12 moods — see `genreVocabulary`/`moodVocabulary` in `providers.go`), with anything the model returns
   outside that list silently dropped. `language` stays open-vocabulary on purpose, since it should reflect the
   track's actual language rather than a curated list. The vocabulary constraint exists specifically so
   [AI Mood Playlists](https://github.com/RFLundgren/ai-mood-playlists)' one-playlist-per-tag-value approach
   doesn't fragment into near-duplicates (`chill`/`relaxed`/`mellow` for the same idea).
4. Tags are written back via `setUserTag.view`.

## Cost & AI provider responsibility

This plugin calls a third-party AI provider (Anthropic, OpenAI, or Gemini) directly using **your own API key**,
configured in the plugin's settings. **You are solely responsible for any usage charges your provider bills to that
key.** Neither this plugin nor Navidrome imposes a spending cap — manage that on the provider's side (Anthropic
Console, OpenAI's usage dashboard, Google AI Studio / Cloud billing).

Before running this against a large library:

- Check your provider's current pricing for whatever model you've configured — this changes often enough that any
  number quoted here would go stale.
- Consider a budget alert or hard spending cap in your provider's billing dashboard, if it offers one.
- Test with a small `maxTracksPerRun` first to confirm cost and tag quality before scanning your whole library.
- Free-tier API keys often have very low requests-per-minute limits (e.g. 5/min has been observed on Gemini's free
  tier) — if classification seems to crawl or fail with `429`/quota errors, that's the likely cause, not a bug. The
  task queue's retry backoff is tuned for roughly a 60-second provider rate-limit window.

## Configuration

Set via Navidrome's Admin → Plugins → AI Auto-Tagging → Config, after installing the `.ndp` package:

| Field | Notes |
|---|---|
| `provider` | `anthropic`, `openai`, or `gemini` |
| `apiKey` | The selected provider's API key |
| `model` | Model name for the selected provider — verify the exact ID against your provider's current model list |
| `tagCategories` | Which categories to suggest: any of `genre`, `mood`, `language`. If you're not using [AI Mood Playlists](https://github.com/RFLundgren/ai-mood-playlists) or otherwise don't need language tags, set this to just `genre, mood` — the manifest default includes `language` for anyone installing fresh, but it's a config-only change, no code involved, and won't automatically drop tags already written (see `PLAN.md` for a one-time cleanup approach if you're changing this after already tagging a library) |
| `libraryUser` | Username whose library view is used to read tracks and read/write tags |
| `cron` | Cron expression for how often to scan for untagged tracks |
| `batchSize` | Tracks per classification API call |
| `maxTracksPerRun` | Ceiling on how many tracks are scanned per scheduled run |

The plugin also needs the **Users Permission** grant (Admin → Plugins → AI Auto-Tagging) for whichever user matches
`libraryUser`, since it authenticates its Subsonic calls as that account.

## Relationship to navidrome-experimental

This is a **separate project**, deliberately not built inside the `navidrome-experimental` repo or its
`plugins/examples/` folder (which is teaching material for the plugin system, not a home for real production
plugins). It only talks to Navidrome through the plugin API — it doesn't need to *be* Navidrome, the same way the
[Cirque](https://github.com/RFLundgren/navidrome_experimental/discussions/15) client and the Pulse companion app
are separate projects from the server they talk to.

That said, this plugin has a real dependency on `navidrome-experimental`: it can't write tags without the
`setUserTag`/`removeUserTag`/`getUserTags` endpoints this project added there. Track further core-fork work in
`navidrome-experimental`'s own
[FEATURE_ROADMAP.md](https://github.com/RFLundgren/navidrome_experimental/blob/master/FEATURE_ROADMAP.md), not here.

## Building

```bash
tinygo build -o plugin.wasm -target wasip1 -buildmode=c-shared .
```

Package into a `.ndp` (the wasm file must be named exactly `plugin.wasm` inside the zip):

```powershell
# Windows
Compress-Archive -Path manifest.json,plugin.wasm -DestinationPath ai-auto-tagging.zip
Rename-Item ai-auto-tagging.zip ai-auto-tagging.ndp
```

```bash
# Linux/macOS
zip -j ai-auto-tagging.ndp manifest.json plugin.wasm
```

Run tests (no TinyGo/WASM runtime needed — the PDK provides mocks for native builds):

```bash
go test ./...
```

See [Navidrome's plugin system docs](https://github.com/navidrome/navidrome/tree/master/plugins) for the general
mechanics (manifest format, permissions, host services, testing with the Extism CLI) — this project only documents
what's specific to *this* plugin here and in [PLAN.md](PLAN.md).

## License

TBD.
