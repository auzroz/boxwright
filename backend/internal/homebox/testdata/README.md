# Homebox response fixtures

Captured from a live **Homebox v0.26.2** instance (commit `e01dd73`) on
2026-09-08, then sanitized. Entity names, descriptions, prices and UUIDs are
replaced with synthetic values; **JSON shape, key presence and value types are
byte-faithful to what the server actually sent**, which is the only property
the tests depend on.

Custom-field *names* (`capacityUnits`, `fragileSafe`, `access`) are our own
schema, not user data, and are deliberately preserved.

| Fixture | Encodes |
|---|---|
| `entity_types.json` | Bare array (not paginated). The two default types: `Item` (`isLocation:false`) and `Location` (`isLocation:true`). |
| `entities_locations.json` | Paginated wrapper. Note `parent` is present only on the one nested location and absent at the root; `fields` is absent from **every** row; `itemCount` appears only where non-zero. |
| `entities_items.json` | Paginated wrapper. `parent` present on every row, `fields` absent from every row. |
| `entity_detail_location_with_fields.json` | The detail `GET`, the only place `fields` appears. Carries all three value types — `number`, `boolean`, `text` — and is the fixture that pins the `CustomField` decoding fix. |
| `entities_empty.json` | An empty collection. The server sends `"items": []`, never `null`. |

## Why `fields` forces an N+1

`fields` was absent from 0/9 location rows and 0/132 item rows in the live
capture, and is present on the detail `GET`. So reading box metadata requires
one request per candidate location. That is a property of the API, not a
choice — the only lever is how many locations we ask about.
