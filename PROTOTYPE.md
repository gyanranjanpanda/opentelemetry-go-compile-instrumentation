# GenAI provider adapter — prototype report

A prototype for an LFX Mentorship proposal: a shared `pkg/genai` core that owns GenAI span
lifecycle, attribute assembly, metrics and streaming, with each provider implementing a narrow
adapter interface that knows nothing about OpenTelemetry.

Branch `prototype/genai-adapter`. The first three commits are in a deliberate order:

| commit | what |
| --- | --- |
| `f94d9d9` | `pkg/genai` — the shared core and adapter contract |
| `73e50b4` | equivalence harness + golden data, recorded from the **unmodified** v3 module |
| `49388f5` | `openai-go/v3` migrated onto the core |
| `2abdac5` | contract fix + two more scenarios, after falsifying the harness (§3.1) |

The order of the first three is the point. The harness was written and committed against the
existing implementation, so its golden data encodes what otelc does today. Written after the
migration it would have encoded what the shared core happens to do and passed by construction.

The fourth commit is the more honest part of the story: the harness was checked by trying to break
it, turned out to have a real hole, and closed it — which exposed a defect in the adapter contract
itself. §3.1 is the account.

Not for upstream merge. `openai-go` v1, v2 and `anthropic-sdk-go` are untouched.

---

## 1. The duplication, measured

The premise checked out, and understates the problem: the three OpenAI modules have not diverged at
all — they are a literal `sed`-able copy.

```
$ diff <(sed 's|openai-go/v3|openai-go|g' .../v3/middleware.go) .../openai-go/middleware.go
4c4
< package v3
---
> package v1
```

Normalising the version string across every shared file:

| file | lines | v2 vs v1 | v3 vs v1 |
| --- | ---: | --- | --- |
| `middleware.go` | 386 | identical but `package` | identical but `package` |
| `semconv/genai.go` | 96 | **byte-identical** | **byte-identical** |
| `streaming_bridge.go` | 34 | identical but `package` | identical but `package` |
| `hook.go` | 56 | `package` + log string | `package` + log string |
| 5 test files | 884 | identical but `package` | identical but `package` |

- **1716** non-test lines across the three OpenAI versions, of which **1144 are duplicates**.
- **2652** test lines, of which **1768 are duplicates**.
- **309** lines already de-duplicated once, in `openai-go/internal/streaming` — a separate module
  that v2 and v3 reach sideways into. Precedent that this project already accepts a shared,
  non-versioned module for GenAI code.
- `anthropic-sdk-go` is **448** non-test lines with the same structure and different JSON structs.

So the drift risk is real but, between the OpenAI versions, unrealised. There is nothing preventing
it; the next fix applied to one copy and not the others starts it.

### Where the copies have already diverged

Between OpenAI and Anthropic, four differences, one of which was not in the original brief:

1. **Metrics.** Anthropic emits `gen_ai.client.operation.duration`. OpenAI has no meter at all.
   Neither emits `gen_ai.client.token.usage`.
2. **Streaming.** OpenAI accumulates SSE into the span. Anthropic passes streaming requests through
   *uninstrumented* (`middleware.go:107`, citing #679).
3. **Attributes.** Anthropic has `top_k` and two cache-token keys; OpenAI has `frequency_penalty`
   and `presence_penalty`. Anthropic folds cache tokens back into `input_tokens` and derives
   `total_tokens`.
4. **`error.type` has diverged in value shape.** OpenAI emits `"429"`
   (`strconv.Itoa(resp.StatusCode)`); Anthropic emits `"429 Too Many Requests"` (`resp.Status`).
   Not previously noted, and it forces a choice on any shared core. See §5.

The host→provider table is an **ordered slice**, with an explicit comment about map-iteration
nondeterminism (#824) and a dedicated regression test. Preserved verbatim, order and all; the only
change is that its fallback now comes from the adapter's own `ProviderID` rather than a hard-coded
string, which is the sole respect in which the OpenAI and Anthropic tables differed in behaviour.

---

## 2. Line counts, before and after

Measured for the `openai-go/v3` module across `73e50b4..49388f5`.

### The v3 module

| | before | after | |
| --- | ---: | ---: | --- |
| `middleware.go` | 386 | **32** | span lifecycle, host table, body handling, streaming all move out |
| `semconv/genai.go` | 96 | **0** | deleted; one shared copy in the core |
| `streaming_bridge.go` | 34 | **0** | deleted; streaming is core-side |
| `hook.go` | 56 | 56 | **unchanged** |
| `adapter/adapter.go` | — | 296 | new: all the OpenAI-specific parsing |
| **non-test total** | **572** | **384** | **−188 (−33%)** |

| | before | after | |
| --- | ---: | ---: | --- |
| `middleware_test.go` | 130 | 0 | moved to `adapter/adapter_test.go` |
| `semconv/genai_test.go` | 113 | 0 | deleted with the package |
| `middleware_integration_test.go` | 546 | 546 | **unchanged, still passing** |
| `testhelpers_test.go` | 68 | 68 | unchanged |
| `hook_test.go` | 27 | 27 | unchanged |
| `adapter/adapter_test.go` | — | 231 | new |
| **test total** | **884** | **872** | **−12** |

The −33% understates the real saving, because a per-version module is now mostly `hook.go` plus a
296-line adapter — and the adapter is the part that actually differs between providers. The
**cross-version** saving is what matters: migrating v1 and v2 the same way would delete another
**~1144 non-test lines** and **~1768 test lines**, since those copies become the same 32-line
`middleware.go` plus an import of the *same* adapter package rather than three copies of it.

### The shared core (new code)

| | lines |
| --- | ---: |
| `pkg/genai` non-test | 1041 |
| `pkg/genai` tests | 935 |
| harness (`equivalence_harness_test.go`) | 498 |
| golden data (10 JSON files) | 1376 |

The core is larger than any single copy it replaces, because it does strictly more: two metrics
OpenAI never had, an extension mechanism, and the presence rules that let one code path serve
operations with different attribute sets. It is not a like-for-like 386→1041 comparison.

**Test totals:** 44 passing in `pkg/genai`, 79 in the v3 module (including the harness).

---

## 3. Harness result

Ten scenarios, each asserted in two independent sections. Spans are compared **attribute for
attribute** — name, kind, instrumentation scope, status code, status description, every attribute
key/type/value, and every event — against golden data recorded from the pre-migration
implementation.

```
--- PASS: TestGenAIEquivalence/unary_chat_completion/spans
--- PASS: TestGenAIEquivalence/chat_empty_choices/spans
--- PASS: TestGenAIEquivalence/chat_response_without_id_or_model/spans
--- PASS: TestGenAIEquivalence/text_completion/spans
--- PASS: TestGenAIEquivalence/embeddings/spans
--- PASS: TestGenAIEquivalence/error_response_429/spans
--- PASS: TestGenAIEquivalence/transport_error/spans
--- PASS: TestGenAIEquivalence/streaming_chat_completion/spans
--- PASS: TestGenAIEquivalence/azure_deployment_path/spans
--- PASS: TestGenAIEquivalence/untraced_unknown_operation/spans
```

**10/10 span sections pass. Zero span attributes changed.** Verified mechanically: re-parsing every
golden file across the migration commit shows `spans_changed=False`.

The last two scenarios were added *after* the migration and are the more interesting half of this
section — see §3.1. Their golden data was still recorded from the pre-migration code, in a
`git worktree` checked out at `73e50b4`, so the ordering property holds for them too.

`middleware_integration_test.go` — the module's pre-existing behavioural spec, 546 lines — passes
**unmodified**.

### The metric delta, recorded as deliberate

Before the migration all ten metric sections were empty. After, nine scenarios gained metrics:

| scenario | `operation.duration` | `token.usage` |
| --- | --- | --- |
| unary chat | 1 point | 2 points (input=10, output=20) |
| chat, empty choices | 1 point | 2 points (input=1, output=1) |
| chat, no id/model | 1 point | 2 points (input=4, output=6) |
| text completion | 1 point | 2 points (input=5, output=50) |
| embeddings | 1 point | **1 point** (input=2 only) |
| 429 error | 1 point | — |
| transport error | 1 point | — |
| streaming chat | 1 point | 2 points (input=5, output=2) |
| azure path | 1 point | 2 points (input=3, output=5) |
| untraced | — | — |

This is the intended addition, not a regression, so the metrics half of the golden data was
re-recorded. The span half was not touched, and the harness has **separate update flags**
(`-update-golden-spans`, `-update-golden-metrics`) writing separate sections precisely so recording
a metric addition cannot quietly rewrite the frozen span baseline.

Three of those rows are load-bearing evidence rather than filler:

- **embeddings gets one token series, not two.** The operation has no output-token concept, so the
  adapter leaves the pointer nil and the core records nothing rather than a zero. This is the
  presence rule working end to end.
- **error scenarios get a duration but no tokens.** A failed call is still measured; it just has
  nothing to report.
- **the untraced scenario gets neither.** The core records nothing for a call it never traced,
  preserving the boundary the per-provider middleware drew.

Metric scope is `go.opentelemetry.io/otelc/instrumentation/github.com/openai/openai-go/v3` — the
module's own scope, not the core's — so per-instrumentation attribution survives the move.

### 3.1 The harness was not good enough, and how that was found

The first version of this harness had eight scenarios and passed 16/16. It was also **wrong**, in a
way worth recording because it is the most transferable lesson here.

The check that exposed it was falsification: deliberately break the adapter and confirm the harness
notices. Two sabotages were tried.

**Sabotage 1** — make `parseEmbeddingResponse` report an output-token count it never had
(`OutputTokens: genai.Int64(0)`). The harness failed loudly, on both the spans *and* the metrics
section. Good.

**Sabotage 2** — replace `reasons := []string{}` with `var reasons []string` in
`parseCompletionResponse`. That is the ordinary Go slip, and it is not cosmetic: nil means "this
operation has no finish-reason concept", so a chat response with `"choices":[]` would silently stop
emitting `gen_ai.response.finish_reasons`, an attribute the old code always emitted. **The harness
passed all sixteen sections.** Only the unit test caught it.

The cause was plain once looked for: `grep -c '"choices":\[\]' equivalence_harness_test.go` → `0`.
No scenario exercised a successful chat response with an empty choices list. The pre-existing
`middleware_integration_test.go:449` *does* use exactly that body, but asserts only
`gen_ai.provider.name`, so it missed it too.

Two scenarios were added to close it:

| scenario | what it pins |
| --- | --- |
| `chat_empty_choices` | a 200 chat response with `"choices":[]` — nil-vs-empty finish reasons |
| `chat_response_without_id_or_model` | a 200 chat response carrying neither `id` nor `model` |

Their golden data was recorded from the genuine pre-migration implementation, via
`git worktree add <dir> 73e50b4`, so the "golden data predates the migration" property survives.
Recording them against the migrated code would have been worthless.

**The second scenario immediately failed**, and the cause was a defect in the contract rather than
in the adapter. The old code emits `gen_ai.response.id = ""` and `gen_ai.response.model = ""` when
the provider omits those fields. `ResponseInfo.ID` was declared `string`, so an adapter had **no way
to express "reported, and it was empty"** — the core's rule was "emit if non-empty", and the
information needed to do otherwise never reached it. The fix was to make `ID` and `Model` `*string`,
matching how `Usage` already worked, and add a `genai.String()` helper. That also *simplified* the
core: three emission rules ("non-empty for strings, non-nil for pointers, non-nil for slices")
collapse into one — **emit exactly what the adapter set**.

Both sabotages were then re-run against the widened harness. Both are now caught:

```
Sabotage: var reasons []string    → FAIL TestGenAIEquivalence/chat_empty_choices/spans
Sabotage: omit empty response id  → FAIL TestGenAIEquivalence/chat_response_without_id_or_model/spans
```

Three things to take from this:

1. **A green equivalence harness proves nothing until you have watched it go red.** Sixteen passing
   sections were hiding a real regression. Falsification, not coverage percentage, is what
   establishes that a golden-data suite has teeth.
2. **A prototype's own report is evidence about its author, not just its subject.** The earlier
   version of this document claimed the empty-`id` difference was found "by reading rather than by
   testing" and moved on. That was true and insufficient: the right response to noticing a gap in
   your test suite is to close it, not to document it as a known limitation.
3. **The gap was in the contract, not the implementation.** The adapter was written correctly and
   still could not reproduce the old behaviour. That is the strongest single argument in §5 for
   scrutinising the interface before building four providers against it.

### What the harness does not cover

Stated plainly, because it bounds the strength of the result above:

- Nondeterministic values are asserted for presence and sign, not exact value:
  `gen_ai.response.time_to_first_token` is normalised to `<positive-int64>`, duration histogram sums
  are dropped (counts are kept). Token-usage sums *are* compared exactly.
- `exception.stacktrace` is dropped from captured events; `exception.type` and `exception.message`
  are compared exactly.
- Scenarios are driven through the middleware directly, not through a woven binary. No end-to-end
  weaving was exercised (see §5, item 9).
- Coverage is scenario-shaped, so it is only ever as good as the scenario list. §3.1 is the worked
  example of that limit and how it was closed.

---

## 4. The adapter interface as built

`pkg/genai/adapter.go` **imports nothing at all** — not even the standard library. A test parses the
file and asserts the empty import set, so no adapter can inherit an OpenTelemetry dependency from
the contract it is written against.

```go
type Adapter interface {
    Provider() ProviderID
    Classify(call Call) Operation          // OperationUnknown means "do not trace"
    ParseRequest(call Call) (RequestInfo, error)
    ParseResponse(call Call) (ResponseInfo, error)
}

// Optional. Providers that return server-sent events implement this too; the
// core owns SSE framing and hands over each decoded payload.
type StreamAdapter interface {
    Adapter
    ParseStreamChunk(payload []byte, acc *ResponseInfo)
}
```

Two design rules do most of the work:

**Presence is the adapter's to declare.** Optional scalars are pointers and `FinishReasons` uses
nil-vs-empty. An adapter says "this operation has no output-token concept" with a nil pointer and
"it reported zero" with a pointer to zero. This is what makes embeddings reproduce exactly: the
existing middleware emits 6 response attributes for chat and only 3 for embeddings, and a core with
a uniform emission set would have added three wrong attributes to every embeddings span.

**Typed field if semconv names it, `Ext` otherwise.** `top_k` and `frequency_penalty` both get
typed fields even though no single provider sends both, because both have `gen_ai.request.*` keys.
Anthropic's cache tokens have no such key and go in `Ext`. Without that line `Ext` becomes a dumping
ground and the core type-switches to emit standard attributes.

---

## 5. What the interface could not express cleanly

This is the section that matters. Nine items, unsoftened.

**1. `ParseStreamChunk` is not told the operation, and that is the signature I would change.**
It takes `(payload []byte, acc *ResponseInfo)` — no `Call`, no `Operation`. It cost nothing here
only by luck: OpenAI's chat and text-completion chunks are identical in every field the
instrumentation reads (the originals `processChatChunk` and `processCompletionChunk` differ solely
in a `delta.content` field neither one uses), so one decode serves both. A provider whose stream
shape varies by operation would have to stash the operation in a closure or smuggle it through the
accumulator. Anthropic is exactly that case — `message_start` nests its payload under `.message`
and its deltas are typed events rather than one repeated shape. **Fix before proposing: pass the
same `Call` here as everywhere else.**

**2. `Classify` gets no body, by design, and that forecloses payload-based classification.**
The core calls `Classify` before reading a single byte, which is right — you should not buffer a
body for a call you will not trace. Both providers today classify on the URL path, so no workaround
was needed. But an API that multiplexes operations over one path cannot be adapted without either a
second classify pass after the bounded read or buffering every request. The contract should say
this out loud rather than leave it implicit in the call order.

**3. The presence rules are load-bearing but unenforced.** Everything in §4 rests on adapter
authors getting nil-vs-zero and nil-vs-empty right. Nothing in the type system makes them.
`Usage{}` and `Usage{OutputTokens: genai.Int64(0)}` produce different spans and only one matches
today's output; the distinction lives in a doc comment.

This one is half-fixed. `ResponseInfo.ID` and `.Model` started as plain `string` with an "emit if
non-empty" rule in the core, which meant an adapter simply **could not** say "the provider reported
an empty id" — a case the old code emitted and the new one dropped. §3.1 has the full story. They
are now `*string`, which both closes the hole and collapses three emission rules into one. What
remains unsolved is enforcement: nothing stops the next adapter author from writing `Usage{}` where
they meant `Usage{OutputTokens: genai.Int64(0)}`. I considered a per-operation attribute manifest in
the core (rejected: moves provider knowledge back into the core, which is the thing being undone)
and a `Reported` bitmask (rejected: uglier than pointers, same failure mode). Still a wart.

**4. Streaming and unary disagree about presence, and I preserved the disagreement.**
The existing streaming finaliser emits `finish_reasons`, `input_tokens`, `output_tokens` and
`total_tokens` *unconditionally*; the unary path emits each only when reported. So the same adapter,
on the same operation, produces a different attribute set depending on whether the response
streamed. The core reproduces this faithfully — `stream.go` carries a comment saying so — because
unifying it would have failed the harness. It is a pre-existing bug that the shared core makes
visible for the first time, and the right follow-up is to fix it and re-baseline the harness
deliberately.

**5. `error.type` had already diverged and the core had to pick a side.** OpenAI's `"429"` won;
Anthropic's `"429 Too Many Requests"` loses. The adapter interface has no say — error handling is
wholly core-side, which is correct, but it means "adopt the shared core" is not a no-op for
whichever provider loses the coin toss. Any real proposal must say which shape is right (`"429"`
matches the semconv guidance for HTTP status codes) and treat the other provider's change as a
deliberate, announced break.

**6. The contract could not express "reported, but empty" — and the harness did not catch it.**
The old chat/completion parser emits `gen_ai.response.id` and `gen_ai.response.model` even when the
provider omits them. With `ID string` on `ResponseInfo`, no adapter could reproduce that: the core's
only available rule was "emit if non-empty". The first eight-scenario harness passed anyway, because
every scenario happened to carry both fields. Found by falsification, fixed by making both fields
pointers, and now pinned by `chat_response_without_id_or_model`. Full account in §3.1.

Worth separating two judgements here. Emitting `gen_ai.response.id = ""` is *bad telemetry* and the
new behaviour would be an improvement — but that is a change to make deliberately and announce, not
one to ship as a silent side effect of a refactor. The contract now supports either choice, which is
the property that actually matters.

**7. `Ext` lets an adapter carry a fact but not emit one.** The extension mechanism is
type-switched *in the core*, so adding a provider-specific attribute still means editing
`pkg/genai`. `CacheUsage` lives in the core, not in an Anthropic package. That is a defensible
trade — it keeps the whole emitted semconv surface reviewable in one file — but it is not the
"providers are self-contained" story the interface superficially suggests, and the proposal should
not claim otherwise.

**8. `Provider()` does double duty.** It supplies both `gen_ai.system` and the host-table fallback
for `gen_ai.provider.name`. That is correct for both providers today and would break for an adapter
whose system name differs from its default provider name. One method, two semantics, no way to
separate them.

**9. Tracer/meter wiring is asymmetric, and a new module needs one line of tooling.**
`OtelMiddleware` passes `WithTracer(tracer)` — because the module's existing tests set a
package-level `tracer` and I wanted `middleware_integration_test.go` to keep passing byte-unchanged
— while the meter is resolved from the global provider via `WithScope`. Cosmetic, but the real
design should settle on one `genai.New(...)` held by the hook.

More substantively: `pkg/` is its own module with **zero** dependencies, so `genai` had to become a
separate module (`pkg/genai`) rather than a package inside it, or every instrumentation in the repo
would inherit the OpenTelemetry dependency tree through `pkg/hook`. But
`tool/internal/setup/sync.go:223-228` **hardcodes** replace directives for exactly two modules,
`otelc/pkg` and `otelc/pkg/runtime`, and nested-module discovery only walks a matched
instrumentation's own directory and its parent. `pkg/genai`'s sources *are* shipped (the whole
`pkg/` tree is zipped into `otelc-pkg.gz` and extracted to `<tmp>/pkg`), but a woven build cannot
resolve the module without one added line:

```go
replaces[util.OtelcPkgRoot+"/genai"] = filepath.Join(util.GetBuildTempDir(), unzippedPkgDir, "genai")
```

Deliberately **not** made here, since the brief ruled out changes outside `pkg/genai` and the v3
module. `go build` and `go test` pass regardless via the local `replace` in v3's `go.mod`; only
end-to-end weaving is affected.

---

## 6. What is stubbed or missing

Against the full design:

**Content capture.** `RequestInfo.Messages` is populated on every chat and completion request and
**never emitted**. `pkg/genai/semconv.go` and the adapter both carry a
`// gated at emission; see proposal §4.5` marker, and a unit test
(`TestHTTPMiddleware_MessagesNeverEmitted`) asserts no span attribute contains message content. The
field exists to prove the contract can carry it; the gating, redaction and opt-in are not built.

**Events.** No `gen_ai.*` log events or message span events. Only `RecordError`'s exception events,
which are pre-existing.

**Other versions and providers.**
- `openai-go` v1 and v2 are untouched and still carry their own copies plus `internal/streaming`.
  The migration is mechanical from here: their `middleware.go` becomes the same 32 lines and they
  import the *same* adapter package, but it is not done.
- `anthropic-sdk-go` is untouched. No Anthropic adapter exists. `CacheUsage` is defined and
  unit-tested against a stub adapter, so the extension mechanism is demonstrated, but nothing
  parses a real Anthropic response through it.
- Gemini, MCP and LangChain: out of scope, not attempted.

**Non-HTTP attachment.** `Call` avoids `http.Header` in its field types (it uses
`map[string][]string`) specifically so the contract does not presuppose HTTP, but `HTTPMiddleware`
is the only attachment point that exists. An SDK-level hook — Bedrock, Vertex — would need a second
entry point, and whether the adapter contract survives that transition untested is the single
biggest open question in the design.

**Smaller gaps.**
- `RequestInfo.Stream` is parsed and unused: the core decides streaming from the response
  `Content-Type`, as the existing code did. A provider that only signals streaming in the request
  would need the core to consult it.
- `gen_ai.client.token.usage` uses default histogram buckets, not the explicit boundaries semconv
  advises for either metric.
- No `gen_ai.request.seed`, `choice.count`, `stop_sequences`, or `server.address`/`server.port` —
  matching today's behaviour rather than closing the semconv gap.
- The core adds a `resp.Body == nil` guard the OpenAI middleware lacked (it would have panicked).
  Strictly a fix; unexercised by the harness.
- `pkg/runtime` gained no dependencies, per the brief.

---

## 7. Reproducing

```sh
# core
cd pkg/genai && go test ./...

# migrated module, including the equivalence harness
cd instrumentation/github.com/openai/openai-go/v3 && go test ./...

# the harness alone, per-scenario (use -v: passing subtests print nothing without it)
go test -run TestGenAIEquivalence . -v
```

### Checking that the harness proves what it claims

Two properties carry the whole argument, and both are checkable.

**The golden data predates the migration.**

```sh
V=instrumentation/github.com/openai/openai-go/v3

git show 73e50b4 --stat -- $V/testdata/equivalence   # 8 goldens added pre-migration
git diff 73e50b4 49388f5 -- $V/equivalence_harness_test.go   # empty: harness never changed

# no span attribute moved when the implementation swapped underneath it
python3 - <<'EOF'
import json, subprocess, os
b = 'instrumentation/github.com/openai/openai-go/v3/testdata/equivalence'
for f in sorted(os.listdir(b)):
    old = subprocess.run(['git','show',f'73e50b4:{b}/{f}'],capture_output=True,text=True).stdout
    if not old:
        print(f'{f:40s} (added later, see PROTOTYPE.md 3.1)'); continue
    print(f'{f:40s} spans_changed={json.loads(old)["spans"] != json.load(open(f"{b}/{f}"))["spans"]}')
EOF
```

The two scenarios from §3.1 are absent from `73e50b4` by construction. Their goldens were recorded
the same way, in a worktree:

```sh
git worktree add /tmp/pre-migration 73e50b4
# add the scenario there, run with -update-golden-spans, copy the JSON forward
git worktree remove /tmp/pre-migration
```

**The harness has teeth.** Do not trust a green suite you have not seen fail. Break the adapter and
confirm it notices — these are the two from §3.1, and both should fail:

```sh
cd instrumentation/github.com/openai/openai-go/v3
cp adapter/adapter.go /tmp/adapter.bak      # NOT git checkout: the baseline may be uncommitted

# 1. nil instead of empty slice -> chat_empty_choices/spans must FAIL
sed -i '' 's/reasons := \[\]string{}/var reasons []string/' adapter/adapter.go
go test -run TestGenAIEquivalence . -v 2>&1 | grep FAIL
cp /tmp/adapter.bak adapter/adapter.go

# 2. drop an empty response id -> chat_response_without_id_or_model/spans must FAIL
#    (edit parseCompletionResponse so ID is nil when resp.ID == "")
```

### Checking the report's claims

```sh
# "adapter.go imports nothing"
(cd pkg/genai && go test -run TestAdapterFileImportsNothing ./...)

# "otelc.yaml and hook.go unchanged"; "pkg/runtime and tool/ untouched"  (both expect: empty)
git diff a8e29a7..HEAD -- $V/otelc.yaml $V/hook.go
git diff a8e29a7..HEAD --stat -- pkg/runtime tool/

# every module still builds, including the ones this did not touch
for m in . pkg pkg/runtime pkg/genai \
  instrumentation/github.com/openai/openai-go{,/v2,/v3,/internal/streaming} \
  instrumentation/github.com/anthropics/anthropic-sdk-go; do
  printf "%-62s " "$m"
  (cd "$m" && go build ./... >/dev/null 2>&1 && go test ./... -count=1 >/dev/null 2>&1) \
    && echo OK || echo FAIL
done
```
