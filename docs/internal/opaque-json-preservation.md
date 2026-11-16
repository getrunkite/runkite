# Opaque JSON numeric preservation

Unreleased, source checkout only. Published v0.9.0 does not include this fix.

Opaque agent capabilities and registry metadata must survive a read and write
without silently changing numeric values. Standard Go interface decoding turns
`9007199254740993` into `9007199254740992` and `1.0000000000000001` into `1`.
The same conversion happens when a browser parses a registry response before
submitting the loaded metadata with a visible-field edit.

All SQLite, Postgres, and MySQL current, search, and immutable-version capability
and registry metadata readers now use the existing `jsonvalue.Unmarshal` helper.
Only those opaque fields change decoding. Typed counters and unrelated control
metadata keep their existing behavior. Registry public and Admin PUT decode the
bounded request body with that helper as well. The body size bound, single JSON
document rule, and encoding/json duplicate-key behavior remain in effect.
Malformed nonempty stored control maps fail their SQL read with a generic error,
so a caller cannot mistake corrupted capabilities for an empty map and overwrite
them during bootstrap. Legacy empty database values and JSON null remain nil maps.

Admin entry views add `metadata_json`, a JSON string produced from the stored
metadata. Public registry models and response fields are unchanged. The browser
keeps that string in the form and validates it as one object or null. The raw PUT
builder inserts the original fragment at a fixed metadata property. Neither the
builder nor the explicit raw-JSON API write serializes parsed metadata. Normal
cookie authentication, CSRF, same-origin paths, and redirect rejection remain in
the shared API client. No metadata is added to audit events or error messages.

A loaded entry without `metadata_json` cannot be saved. This fails closed when a
new browser talks to an older replica, including when parsed metadata appears
absent. New entries still omit metadata. Existing null and empty-object metadata
remain null and empty objects. PUT continues to replace the submitted entry;
omitting metadata in an API request clears it. There is no metadata-preserve
query, merge mode, conditional write, or new concurrent-edit protection. A stale
form can still replace a newer server value, as before.

Admin Agent and Registry exact selectors retain the supplied tenant ID without
trimming. An explicitly empty `tenant_id` selects the default tenant and masks
inherited system authority; omitting the query retains global Admin API lookup
and version-history behavior. Registry mutations always mask system authority.
The form keeps an existing entry's exact immutable route name and loaded tenant,
including padding that distinguishes neighboring keys. Route or tenant selection
changes remount its request, form and delete-confirmation state. Save also rejects
an unhydrated form or a form whose record no longer matches that selection. New
entry creation retains its existing slug cleanup. These changes do not add a
general identifier normalization or validation policy.

Registry creation and editing use explicit modes. Existing entries link to
`/admin/registry/{name}/edit`, so an API entry literally named `new` is editable.
The legacy `/admin/registry/new` creation URL and other legacy detail URLs remain
available. Mode participates in the form remount key and save guard, preventing
loaded metadata from crossing into a creation form. REST paths are unchanged.

The guarantee follows storage precision. SQLite text and Postgres JSONB preserve
these decimal values within their existing JSON limits. Postgres may normalize
numeric spelling, so this promises numeric values rather than original whitespace
or exponent spelling. MySQL control fields remain native JSON columns. Their
integer range can retain integers beyond JavaScript's safe range, but native JSON
can round arbitrary decimal fractions before a reader sees them. This patch does
not migrate columns or promise arbitrary decimal preservation on MySQL.

Mongo control metadata keeps its existing semantics. The registry ingress now
produces json.Number for SQL, so Mongo converts only those number leaves to its
previous float64 HTTP-ingress representation before BSON writes. Existing native
BSON scalar types supplied directly to the store retain their types. The exact
application JSON codec is not applied to catalog metadata or capabilities. Large
or precise native BSON metadata cannot be asserted exact after an HTTP roundtrip;
choose SQLite or Postgres when exact opaque JSON numbers are required. Previously
rounded values cannot be recovered by this change.

Qualification covers supported-range numeric values through every SQL current,
search, and immutable-version read, plus read and re-upsert without a new agent
version. SQLite and Postgres exact suites add decimals that round to integers in
binary64, large integers, nested arrays, null, booleans, and numeric strings.
In-process API regressions exercise raw public PUT, Admin GET/string transport,
Admin replacement PUT, public GET and immutable history without adding an Admin
field to the public API. Browser-module tests deliberately round parsed metadata,
then capture the actual shared client's fetch body and verify that its metadata
fragment remains byte-for-byte equal to the loaded string, with CSRF and cookie
options present. Additional tests reject invalid fragments and older responses.
Native BSON encode/decode tests verify legacy ingress conversion and preservation
of direct int64 and Decimal128 values. These are not live Mongo server or browser
UI qualification claims. The capability bootstrap caller needs its separate
read/merge/write regression when integrated with the capability catalog change.
Real Admin handler tests also cover exact padded and empty-default tenant
selection under inherited system context, global no-query compatibility, and
save/delete isolation from canonical neighbors. Shared-client request tests keep
padded identifiers exact and reject stale form state after record selection.
