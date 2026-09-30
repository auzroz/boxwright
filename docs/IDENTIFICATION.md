# Which model identifies items

The Anthropic provider's default (`claude-sonnet-5-5`, with its measured
thinking mode) is chosen by measurement, not by argument. This file holds the
latest measurement and how to repeat it. Re-run it whenever a model is
released; do not edit the default by hand.

## Recommendation (2026-09-30)

**`AI_MODEL=claude-sonnet-5-5`, `AI_EFFORT` and `AI_THINKING` unset** (the
model's own high effort, adaptive thinking). It is now the default, so
`AI_PROVIDER=anthropic` plus a key is the whole configuration.

Against the previous default, `claude-opus-5`, it found **every** labelled
item where Opus 5 found 88%, never answered with a placeholder (Opus 5 did,
twice), got categories right more often, invented nothing, answered in half
the time, and cost **$0.016 a photo instead of $0.041**.

## How it was measured

`backend/cmd/identeval`: 20 openly licensed Wikimedia Commons photos
(`testdata/sources.json`, fetched at 1024 px and pinned by sha256; no image is
in the repository) with hand-written labels (`testdata/labels.json`), put
through the **same provider code the server uses**. The set covers single
items, repeated items that must be counted, a few different things, dense
scenes (a pantry shelf, a cluttered desk, a flat-lay of about 40 hand tools),
an awkward shape (tangled string lights), something too big for a container
(a lawn mower) and a printed label to read (a board game).

For each answer:

| Column | Meaning |
|---|---|
| Recall | Of the labelled must-find items, how many it named. Keywords, case-insensitive; lenient on purpose |
| Category | Of the items found, in an acceptable category |
| Count | Of the items found, quantity within the labelled range |
| Size / given | Longest side within range, where the real size is known; and how often a size was offered |
| Flags | Bulky and fragile, where labelled |
| Extras | Named things that match nothing labelled: invented, or real but unlabelled |
| Stubs | Answers that were a placeholder ("placeholder", "x") instead of a list |
| Stable | Photos whose recall did not change between repetitions |

## Results

300 calls: every config once over all 20 photos, then the five closest
contenders a second time. $7.45 at list price. Latency is wall time from this
Mac to the API; cost is list price per photo.

| Config | Recall | Category | Count | Size | Flags | Extras/photo | Stubs | p50 s | p90 s | Out tok | $/photo | Runs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| **Sonnet 5.5, high, adaptive (its default)** | **100%** | **98%** | 92% | 100% | 100% | **0.0** | 0 | 3.6 | 12.7 | 675 | **0.0161** | 40 |
| Sonnet 5.5, medium, between_tools | 100% | 93% | 96% | 100% | 100% | 0.1 | 0 | 3.6 | 12.3 | 728 | 0.0167 | 40 |
| Sonnet 5.5, low, between_tools | 99% | 93% | 94% | 100% | 100% | 0.1 | 0 | 4.2 | 13.6 | 776 | 0.0172 | 40 |
| Sonnet 5.5, low, adaptive | 96% | 98% | 94% | 100% | 100% | 0.0 | 0 | 3.5 | 8.1 | 565 | 0.0150 | 40 |
| Fable 5.1, low | 100% | 100% | 93% | 100% | 100% | 0.1 | 0 | 8.6 | 22.7 | 610 | 0.0775 | 20 |
| Sonnet 5 | 100% | 98% | 89% | 80% | 100% | 0.4 | 0 | 5.1 | 16.6 | 749 | 0.0169 | 20 |
| Opus 5.5, medium (its default) | 90% | 96% | 94% | 100% | 100% | 0.2 | 1 | 8.2 | 15.9 | 742 | 0.0336 | 20 |
| Opus 5 (previous default) | 88% | 95% | 94% | 89% | 86% | 0.4 | 2 | 7.1 | 21.6 | 710 | 0.0412 | 40 |
| Haiku 4.5 | 80% | 92% | 88% | 80% | 86% | 0.4 | 0 | 2.7 | 6.7 | 400 | 0.0056 | 20 |
| Opus 5.5, low | 73% | 91% | 96% | 100% | 100% | 0.2 | 1 | 4.7 | 7.8 | 391 | 0.0266 | 20 |

Where the differences live: every config handled the single items, the
books, the lawn mower (as bulky) and the board game (read by name). The
separation is entirely in the crowded photos -- the pantry shelf, the tool
flat-lay, the paint cans, the sewing basket.

## What it found

- **The Opus models sometimes give up on a crowded photo.** Opus 5 answered
  the tool flat-lay and the paint cans with ONE item named "placeholder", in
  about 90 output tokens; Opus 5.5 did the same with "x" twice, and at low
  effort listed only the claw hammer out of about 40 tools. The schema was
  satisfied, so nothing failed: the user would have been handed a draft named
  "placeholder". No Sonnet, Haiku or Fable answer did this.
- **Thinking no longer costs what it did.** The rule "decline thinking for
  extraction" (measured on the Opus 5 generation: 1212 extra output tokens,
  no better answer) does not hold for Sonnet 5.5: adaptive thinking at its
  default used FEWER output tokens than `between_tools` and was more accurate
  on categories. So `AI_THINKING=auto` now means the measured best per model
  family (adaptive for Sonnet 5.5) and `least` keeps the old behaviour.
- **Input is now most of the bill.** The prompt with the category vocabulary
  is about 4,700 input tokens against about 700 out, so input is ~58% of the
  cost. The earlier "77% is output" was true of a shorter prompt.
- **Latency tracks the number of things**, as before: Sonnet 5.5 takes 2.3 s
  at the median for one item and 13 s for a dense scene of ~19 items; Opus 5
  took 5.8 s and 27 s.
- **Fable 5.1 is as accurate as Sonnet 5.5**, at 4.8x the cost and 2.4x the
  latency. Nothing here needs it.

## Limits

- Twenty stock photos, lit and framed better than a storage unit. They rank
  models; they do not predict a success rate on your shelves.
- One or two runs per config. The top four Sonnet 5.5 rows are within noise
  of each other on recall; the adaptive default wins on categories and extras
  across both runs.
- Item crops (`region`) are not scored. Sonnet 5 was verified to place them
  correctly; the 5.5 models have not been checked on that yet.
- The labels were written by one person looking at each photo. A result that
  looks wrong is as likely a label as a model: read the answers in the
  `.jsonl`, fix the label, and `-rescore` (free).

## Re-running it

```
AI_API_KEY=$(op read op://<vault>/<item>/credential) make identeval
make identeval ARGS='-configs "Sonnet 5.5" -runs 1'         # a subset
make identeval ARGS='-resume backend/cmd/identeval/results/<run>.jsonl'
cd backend && ./bin/identeval -rescore cmd/identeval/results/<run>.jsonl
```

When a model is released:

1. Add it to `backend/cmd/identeval/testdata/matrix.json`, at its default and
   at a lower effort.
2. Add its price to `anthropicPricing` and, if it is a new family, a row to
   `anthropicFamilies` (`backend/internal/ai/`) saying whether it takes
   `effort` and how its thinking is turned down.
3. Run it, update this file, and change `anthropicDefaultModel` (and the
   family's `adaptiveByDefault`) only if the table says so.

Results land in `backend/cmd/identeval/results/` (not committed): a `.jsonl`
of every answer, a `.md` report and a `.summary.json`.
