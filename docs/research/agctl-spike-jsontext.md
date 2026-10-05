# Byte-exact `~/.claude.json` rewriting with `encoding/json/jsontext`

Verdict: **GO**. Written 2026-10-05 17:54 JST (from `date`); test and probe
output below was produced the same day on go1.27.1 darwin/arm64.

The live-config rewrite — replace `oauthAccount`, delete the five stale
caches, keep every other byte — can be done in Go with the stdlib
`encoding/json/jsontext` decoder plus a ~90-line canonical pretty-printer,
and it reproduces the Rust reference's behaviour on every corpus case,
including the number-spelling refusals. Implementation:
`internal/provider/claude/claudejson.go`; corpus:
`internal/provider/claude/claudejson_test.go` + `testdata/`.

## What the Rust reference actually does

`src/provider/claude/claude_json.rs:277-297` (`reproduce`) does **not**
splice spans: it parses into a `serde_json` `preserve_order` map, refuses
unless `serde_json::to_string_pretty` of the unmodified map is byte-identical
to the input, then mutates the map (insert `oauthAccount`, `shift_remove`
the five caches, `claude_json.rs:904-907`) and re-serialises the whole
document. The gate makes whole-document re-serialisation safe: a document it
reproduces is one where re-rendering IS the identity outside the edits.

The Go port keeps the same gate but produces the output by span splicing
(the member spans located during the gate's own canonical rendering), so
bytes outside the replaced/deleted members are copied from the input
verbatim rather than re-encoded. For any document that passes the gate the
two strategies are byte-equivalent; splicing additionally satisfies the
"unchanged document returns the original slice" requirement without a
re-render.

## The formatter decision and its evidence

Claude Code writes the file with `JSON.stringify(_, null, 2)`; the Rust gate
compares against `serde_json` 1.0.151 with `preserve_order` +
`float_roundtrip` (pinned by `claude_json_tests.rs:431-491`). No Go stdlib
formatter reproduces that output: `jsontext.Value.Indent` keeps the original
number and string spellings by design ("It does not reformat JSON strings or
numbers"); a token-for-token `jsontext.Encoder` with `WithIndent` reproduces
the layout and string escaping but passes raw number spellings through and
appends a trailing newline after the top-level value (measured); and
`Value.Format` with `CanonicalizeRawFloats` canonicalises to RFC 8785, whose
ECMAScript `ToString` drops the `.0` from integral doubles (`1.0` → `1`),
which the reference's printer keeps. So the spike implements the minimal
formatter over `jsontext.Decoder` tokens.

Ground truth for the canonical forms came from a scratch crate pinned to
`serde_json = "=1.0.151"`, features `["preserve_order", "float_roundtrip"]`
(the manifest line the Rust test asserts), printing `to_string` /
`to_string_pretty` for a battery of inputs. Measured rules, all encoded in
the Go formatter and its tests:

- Layout: two-space indent, `": "` after names, one member/element per
  line, `{}`/`[]` compact when empty, no trailing newline.
- Strings: `\b \t \n \f \r`, lower-case `\u00xx` for the other controls,
  `\"`, `\\`; everything else raw (`/`, `é`, U+2028/U+2029, emoji, DEL).
  This is exactly RFC 8785 §3.2.2.2 minimal escaping, which is what
  `jsontext.AppendQuote` implements — the probe outputs are byte-identical,
  so the Go side uses `AppendQuote` directly.
- Integer tokens (no `.`/`e`/`E`): kept verbatim while they fit `u64`
  (non-negative) or `i64` (negative). `-0` is the one exception: serde
  parses it as the double `-0.0` and writes `-0.0`. Out-of-range integer
  tokens take the double path (`18446744073709551616` →
  `1.8446744073709552e+19`).
- Doubles: shortest round-trip digits (Go's `strconv.AppendFloat(..., -1, 64)`
  produces the same unique shortest digits as Rust's ryu); plain decimal
  while the decimal point position `k` satisfies `-5 < k <= 16`, with
  integral values ending in `.0` (`1e15` → `1000000000000000.0`, `1.0` →
  `1.0`); scientific otherwise as `d.ddde±x` with an explicit `+` and no
  zero-padding (`1e21` → `1e+21`, `0.000001` → `1e-6`, `5e-324` →
  `5e-324`). Float overflow (`1e999`) is a parse error in serde and is
  refused as unparseable in Go; underflow (`1e-999`) parses as `0.0` and so
  can never reproduce its own spelling.
- Duplicate member names: serde keeps the last value at the first position,
  so the canonical rendering always has fewer members than the input and
  the gate always refuses. `jsontext`'s decoder rejects duplicates outright
  (`jsontext.ErrDuplicateName`), which the Go side maps to the same
  not-reproducible refusal — behaviourally identical on every input.
- A lone surrogate, invalid UTF-8, a BOM, truncated input and trailing data
  are parse errors in both (`jsontext` default options reject all four).

## jsontext / json v2 APIs relied on

Verified with `go doc encoding/json/jsontext <Symbol>` on go1.27.1:

- `func NewDecoder(r io.Reader, opts ...Options) *Decoder` — default
  options already reject duplicate names, invalid UTF-8 and lone
  surrogates, which the gate wants.
- `func (d *Decoder) PeekKind() Kind` — structure dispatch without
  consuming; returns KindInvalid on a pending error.
- `func (d *Decoder) ReadToken() (Token, error)` — object/array framing and
  member names; "the returned token is only valid until the next Peek,
  Read, or Skip call", so the decoded name is taken from `Token.String()`
  immediately (violating this panics at run time).
- `func (d *Decoder) ReadValue() (Value, error)` — scalars; "contains the
  exact bytes of the input", which is what lets number tokens keep their
  spelling and the splicer stay verbatim.
- `func (d *Decoder) InputOffset() int64` — available for span capture; the
  implementation instead records spans in its own canonical rendering,
  which the gate proves equal to the input, because `InputOffset` points
  after the most recent token and leading whitespace would have to be
  trimmed per span anyway.
- `func AppendQuote[Bytes ~[]byte | ~string](dst []byte, src Bytes) ([]byte, error)` —
  canonical string escaping (RFC 8785 minimal), probe-identical to serde.
- `func AppendUnquote[Bytes ~[]byte | ~string](dst []byte, src Bytes) ([]byte, error)` —
  decoding the raw string before re-quoting canonically.
- `var ErrDuplicateName error` — classifying the duplicate-name refusal.
- `func (t Token) String() string` — the unescaped member name.

`Decoder.StackDepth`/`StackIndex` and `jsontext.Value.Indent` were examined
and not needed.

## The API later lanes call

```go
const OAuthAccountKey = "oauthAccount"
var StaleCaches = [5]string{...}

func Reproduce(document []byte) error
func Plan(document []byte, account jsontext.Value) (*Rewrite, error)
func (r *Rewrite) Updated() []byte
func (r *Rewrite) Changed() bool
func (r *Rewrite) Apply(current []byte) ([]byte, error)
```

Refusals are the typed sentinels `ErrUnparseable`, `ErrNotAnObject`,
`ErrNotReproducible` (matching the reference's three gate reasons) and
`ErrChanged`. They are returned bare: the decoder's own error messages quote
document content (a lone-surrogate error echoes the surrounding bytes), so
they are dropped rather than wrapped, keeping the reference's rule that a
config error names a reason and never a value — and a test plants a
token-shaped marker in refused documents and asserts no refusal carries it. The intended locked-write sequence: read the file, `Plan`,
take the lock, re-read, `Apply(reRead)` — which refuses with `ErrChanged`
unless the re-read is byte-identical to what the plan was built from — then
write `Updated()` to a temporary file and rename. `Plan` does no I/O and
takes no lock; digest and lock rechecks stay with the caller. The
replacement value arrives as a `jsontext.Value` in any valid spelling and
is re-encoded canonically at the member's depth; building that value from a
profile document (the reference's `build_oauth_account`,
`claude_json.rs:319-364`) is deliberately out of this package: it needs the
profile type a later lane owns.

## Corpus parity

Every behavioural case in `claude_json_tests.rs:292-1055` that concerns the
gate, the transform or the output bytes is translated, with hand-spelled
expected documents (never produced by the code under test) and failures
that print the first differing offset with a hex window on both sides:

- the 166-member byte-identity fixture (fillers with nested containers,
  Japanese text, emoji, escapes, 13-digit stamps, floats, negatives; the
  five caches at first/middle/last positions; `oauthAccount` mid-document);
- the twelve guard rows (trailing newline, CRLF, 4-space indent, `é`,
  `\/`, `1e21`, `0.000001`, duplicate key, lone surrogate, compact, BOM,
  top-level array) as byte-exact `testdata/` files, plus the empty file;
- the reproducible spellings (`1e+21`, `1.0`, `1e-7`, `0.5601675`, `-17`,
  `1789000000000`) and the eight 16/17-digit doubles carried end to end;
- replace-never-merge (the previous account's six extra members and two
  stale optionals do not survive), index kept around a deleted cache,
  absent account appended last.

No corpus case behaves differently from the reference. Cases about locking,
backups, budgets, symlinks and the catch-up flow are file-system behaviour
owned by later lanes and are out of this package's scope by design (the
package has no file I/O to test).

One deliberate strengthening beyond the reference: an unchanged document
(identical account, no caches present) returns the original slice itself —
the reference re-serialises and rewrites the file unconditionally
(`claude_json_tests.rs:1327-1335`); the Go `Rewrite.Changed()` lets the
caller keep that always-write behaviour while proving no byte moved.
