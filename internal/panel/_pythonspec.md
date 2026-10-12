# Python panel test suite — HTTP/backend behavioral specification

Source: `panel/test_*.py` (11 files). This document records only what the tests assert
about the HTTP/backend boundary. Frontend JS rendering assertions are noted as such
and excluded from the PARITY CHECKLIST except where a backend field feeds them.

Reference implementation constants (from `panel/app.py`) used throughout:

| Name | Value |
|---|---|
| `COOKIE_NAME` | `wb2a_admin` |
| `SESSION_TTL` | `30*24*3600` (2592000 s) |
| `LOGIN_MAX_FAILS` | `6` |
| `LOGIN_WINDOW` | `300.0` s |
| `LOGIN_BUCKET_LIMIT` | `10000` |
| `LOGIN_GLOBAL_WINDOW` | `60.0` s |
| `LOGIN_GLOBAL_MAX_FAILS` | `20` |
| `UID_RE` | `^[A-Za-z0-9_-]{1,80}$` |
| `AUTH_FILE_RE` | `^workbuddy(?P<uid>.*)\.json(?P<disabled>\.disabled)?$` |
| `REALMS` | `("cn","global","trae")` |
| `TRUSTED_PROXIES` default | `127.0.0.0/8,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,fc00::/7` |
| `ADMIN_ALLOW_CIDRS` | env `WB2API_ADMIN_ALLOW_CIDRS`, default `""` |
| `TASK_ENABLE_KEY` | `checkin, trae_check, travel, activity, keepalive, school, cat, redeem, lottery, makeup` |
| `_send` default response headers | `Content-Type`, `Content-Length`, `Cache-Control: no-store`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: same-origin`, `X-Frame-Options: SAMEORIGIN`, `Content-Security-Policy: frame-ancestors 'self'; base-uri 'self'; form-action 'self'` |

---

## Shared test fixtures (defined in `test_security_boundaries.py`, imported by several files)

### `QuietServer(ThreadingHTTPServer)`
- `daemon_threads = True`
- `handle_error(request, client_address)` → **no-op** (silences tracebacks).

### `SessionHandler(app.Handler)`
- Overrides `_session(*args, **kwargs)` → returns `{"u": "fixture-admin"}` **iff** the
  request has header `Cookie: fixture-session`, else `None`. **Bypasses real session
  signature checks entirely.**
- `log_message(*args)` → no-op.

### `RealSessionHandler(app.Handler)`
- `log_message(*args)` → no-op. **Uses the real `app.Handler._session`** (real HMAC
  session tokens, real `credentials.json`).

### `test_ops_management.FixtureHandler(app.Handler)`
- `_session()` → `{"u": "fixture-admin"}` iff `Cookie: fixture-session`, else `None`.

### `test_update.Handler(app.Handler)`
- `_session()` → `{"u": "test-admin"}` iff `Cookie: test-session`, else `None`.

---

## 1. `test_body_length_validation.py`

**Server start:** `setUpClass` → `QuietServer(("127.0.0.1", 0), SessionHandler)`, background
`serve_forever` thread (daemon). `tearDownClass` → `shutdown()`, `server_close()`, `join(3)`.
**No patches/monkeypatches at class level. No env vars.** Each test patches
`app.Handler.service_restart` individually.

**Helper `raw_post(length_bytes, body=b"{}")`:** raw TCP socket, sends verbatim:
```
POST /api/service/restart HTTP/1.1
Host: 127.0.0.1
Content-Type: application/json
X-Admin-Request: 1
Connection: close
Content-Length: <length_bytes verbatim, may be non-ASCII>

{}
```
Reads until EOF, returns all bytes.

### T1.1 `test_latin1_digit_byte_is_rejected_as_400_without_leaking_the_exception`
- Patch: `app.Handler.service_restart` (assert `not_called`).
- Input: `Content-Length: <0xB2>` (latin-1 superscript-two byte), body `{}`.
- Expected: raw response contains `HTTP/1.1 400 `.
- Expected NOT present in raw: `HTTP/1.1 500`, `Traceback`, `invalid literal for int`.
- Response body (after first `\r\n\r\n`) is valid JSON with `json.loads(...)["ok"] == False`.
- `service_restart` must **not** be called (body rejected before action).
- Rationale (header comment): `Content-Length` must be validated with ASCII-digit regex,
  not `str.isdigit()` (which accepts latin-1 digit bytes and then makes `int()` raise).

### T1.2 `test_ascii_control_length_still_parses`
- Input: `Content-Length: 2`, body `{}`.
- Expected: raw contains `HTTP/1.1 401 ` (body parses, passes action validation, hits the
  session gate which returns 401).

---

## 2. `test_rejected_body.py`

**Server start:** custom `RejectionGateServer(QuietServer)` instantiated per-test in `setUp`.
- `RejectionGateHandler(SessionHandler)` overrides `_json(code, payload, extra=None)`:
  calls `super()._json(...)`, then if `code in (401, 403)` sets `server.reply_sent`, and
  **blocks until `server.body_sent` is set (3 s timeout, else `RuntimeError`)**. This holds
  the connection open so the client can send a late body after the rejection is written.
- `RejectionGateServer` fields: `reply_sent`, `body_sent`, `request_closed` (`threading.Event`),
  `handler_errors = []`.
- `RejectionGateServer.shutdown_request(request)` → `super().shutdown_request(request)` then
  `request_closed.set()`.
- `RejectionGateServer.handle_error(request, client_address)` → appends to `handler_errors`.
- `tearDown`: `body_sent.set()`, shutdown, `server_close`, `join(3)`, asserts thread dead.
- **No app-level monkeypatches** except per-test `patch.object(app.Handler, "login_start")`
  and `patch.object(app.Handler, "keys_request")`.

**Helper `rejected_request_with_late_body(path, expected, extra_headers)`:**
- Default headers: `Content-Type: application/json`, `Content-Length: 2`,
  `Cookie: fixture-session`, `X-Admin-Request: 1`; `extra_headers` override.
- Sends request line + headers via `putrequest`/`putheader`/`endheaders` **without body**.
- Waits for `reply_sent` (3 s). Then sends body `b"{}"`, sets `body_sent`, waits for
  `request_closed` (3 s). Then `getresponse()`.
- Assertions (all four tests): `response.status == expected`;
  `response.getheader("Connection") == "close"`; `response.read()` is truthy;
  patched actions `assert_not_called()`; `server.handler_errors == []`.

### T2.1 `test_invalid_origin_still_returns_403_with_a_late_body`
- `POST /api/login/start`, `Origin: https://untrusted.invalid`.
- Expected: **403**, `Connection: close`, non-empty body, `login_start`/`keys_request` not called.

### T2.2 `test_missing_management_header_still_returns_403_with_a_late_body`
- `POST /api/login/start`, `X-Admin-Request: ""`.
- Expected: **403**, `Connection: close`, non-empty body.

### T2.3 `test_unauthenticated_key_request_still_returns_401_with_a_late_body`
- `POST /api/keys/copy`, `Cookie: None` (header omitted).
- Expected: **401**, `Connection: close`, non-empty body.

### T2.4 `test_cross_origin_key_request_still_returns_403_with_a_late_body`
- `POST /api/keys/copy`, `Origin: https://untrusted.invalid`.
- Expected: **403**, `Connection: close`, non-empty body.

### RejectedBodyBudgetTests — unit tests of `app.Handler._reject_unread_body`

**Helper `handler(length="2", headers=None)`:** builds `object.__new__(app.Handler)` with
`headers = Message()` (email.message), optional `Content-Length` and extra headers,
`close_connection = False`, `connection = DrainConnection()` (records `settimeout` calls),
`rfile = Mock()`, `_json = Mock()`.

`DrainConnection`: `gettimeout()` returns stored timeout; `settimeout(v)` records `v`.

### T2.5 `test_rejection_is_sent_before_discarding_the_body`
- `rfile.read1(size)` appends `"discarded"` to order, asserts `size == 2`, returns `b"{}"`.
- `_json` side_effect appends `"rejected"`.
- Call `app.Handler._reject_unread_body(handler, 403, {"ok": False})`.
- Expected order: `["rejected", "discarded"]` (rejection written **before** draining).
- `handler.close_connection is True`.
- `_json` called exactly once with first positional arg `403`.

### T2.6 `test_absolute_deadline_is_not_extended_by_slow_trickle`
- `Content-Length: 1000`. `read(size)` asserts `_json` already called, `connection.timeout`
  is not None, `> 0`, `<= 0.1000001`; appends size; asserts `len(reads) <= 7`
  (per-read timeout must not extend the total deadline); if `remaining < 0.015` raises
  `TimeoutError`, else advances clock by 0.015 and returns `b"x"`.
- Patches `app.time.monotonic` with `side_effect=lambda: clock[0]` (clock starts at 1000.0).
- Call `_reject_unread_body(handler, 403, {"ok": False})`.
- Expected: `len(reads) > 0`; total elapsed `clock[0] - 1000.0 <= 0.1000001`;
  `close_connection is True`.

### T2.7 `test_discard_never_exceeds_64kib_even_with_a_huge_declared_length`
- `Content-Length: 1000000000`. `read(size)` asserts `size > 0`, `size <= 4096`,
  accumulates `consumed[0]`, asserts `consumed[0] <= 65536`, returns `b"x"*size`.
- Patches `app.time.monotonic` → `1000.0`.
- Expected: `consumed[0] == 65536`; `close_connection is True`.

### T2.8 `test_invalid_or_ambiguous_lengths_do_not_wait_for_a_body`
- Cases `(length, extra_headers)`:
  `(None,[])`, `("invalid",[])`, `("-1",[])`, `("+2",[])`, `("2.0",[])`, `("0",[])`,
  `("9"*5000,[])`, `("2",[("Content-Length","2")])`, `("2",[("Content-Length","3")])`,
  `("2",[("Transfer-Encoding","chunked")])`.
- For each: `_json` called exactly once; `rfile.read1` **not called**; `close_connection is True`.
- (i.e. only a single, ASCII-digit-only, non-empty `Content-Length` with no
  `Transfer-Encoding` triggers draining.)

### T2.9 `test_peer_disconnect_or_read_timeout_does_not_replace_rejection`
- For `ConnectionResetError`, `TimeoutError`, `ValueError` raised by `rfile.read1`:
  `_json` called exactly once with code `403`; `close_connection is True`.
- (Peer errors must not replace or duplicate the rejection.)

---

## 3. `test_login_throttle_429.py`

**Server start:** `setUpClass` → `QuietServer(("127.0.0.1", 0), SessionHandler)`, daemon
thread. No env vars. Patch: `app.login_global_retry_after` (per test).

**Helper `raw_login(body=b'{"username":"admin","password":"secret"}')`:** raw socket, sends:
```
POST /api/auth/login HTTP/1.1
Host: 127.0.0.1
Content-Type: application/json
X-Admin-Request: 1
Connection: close
Content-Length: <len(body)>

<body>
```
Reads until EOF.

### T3.1 `test_throttled_login_returns_exactly_one_well_formed_429`
- Patch `app.login_global_retry_after` → `42`.
- Expected:
  - `re.findall(rb"HTTP/1\.1 (\d{3})", raw) == [b"429"]` — **exactly one response line**.
  - raw contains `Retry-After: 42`.
  - raw contains `\r\n\r\n` (headers terminated).
  - body after `\r\n\r\n` is non-empty and contains `"ok": false` (with the space after
    the colon, i.e. `json.dumps` default separators).
  - body does NOT contain `unpack` or `Traceback` (no leaked Python exception).
- Response body message (from app.py): `"登录尝试过于频繁，请 42 秒后再试"`.
- Rationale: `extra` must be a list of `(name, value)` tuples; passing a dict made
  `_send` iterate key strings, raising `ValueError` and emitting a second 500.

---

## 4. `test_login_hardening.py`

**No HTTP server.** Direct calls to module-level functions. One test uses env var +
`importlib.reload(app)`.

### `BucketKeyTests`
### T4.1 `test_ipv6_addresses_share_a_64_bucket`
- `app._bucket_key("2001:db8:1:2:3:4:5:6") == app._bucket_key("2001:db8:1:2:ffff:ffff:ffff:ffff")`.
- Both equal `str(ipaddress.ip_network("2001:db8:1:2::/64"))` → `"2001:db8:1:2::/64"`.
- `!= app._bucket_key("2001:db8:1:3::1")`.

### T4.2 `test_ipv4_and_garbage_keys`
- `app._bucket_key("198.51.100.7") == "198.51.100.7"`.
- `app._bucket_key("not-an-address") == "not-an-address"`.

### T4.3 `test_rotating_addresses_hit_one_bucket`
- Clears `app._fails`; calls `app.login_failed("2001:db8:9:9::%x" % i)` for
  `i in range(app.LOGIN_MAX_FAILS)` (6).
- `app.login_blocked("2001:db8:9:9::dead")` → `blocked is True`, `hits == app.LOGIN_MAX_FAILS`.

### `GlobalThrottleTests` (setUp/tearDown clear `_fails` and `_global_fails`)
### T4.4 `test_distributed_failures_are_throttled`
- Initially `app.login_global_retry_after() == 0`.
- `login_failed("198.51.100.%d" % (i % 254 + 1))` for `i in range(LOGIN_GLOBAL_MAX_FAILS)` (20).
- Then `login_global_retry_after() > 0`.
- After `app._global_fails.clear()` → `== 0` (window expiry is the only reset; a
  successful login only clears its own bucket).

### T4.5 `test_below_the_cap_stays_open`
- `login_failed("198.51.100.5")` × `(LOGIN_GLOBAL_MAX_FAILS - 1)` (19) → `login_global_retry_after() == 0`.

### `AllowlistTests` (env var + reload)
Helper `_with_cidrs(value)`: sets `os.environ["WB2API_ADMIN_ALLOW_CIDRS"] = value`,
`importlib.reload(app)`; cleanup pops env and reloads.

### T4.6 `test_unset_allows_everyone`
- `WB2API_ADMIN_ALLOW_CIDRS=""` → `app.admin_client_allowed(FakeHandler("203.0.113.9")) is True`.

### T4.7 `test_configured_allows_only_listed_networks_and_loopback`
- `"203.0.113.0/24"` → allowed for `203.0.113.50`; allowed for `127.0.0.1`;
  denied for `198.51.100.7`.

### T4.8 `test_invalid_entries_are_ignored`
- `"bogus, 203.0.113.0/24"` → allowed `203.0.113.50`; denied `198.51.100.7`.

`FakeHandler`: `client_address = (peer, 12345)`, `headers = {}`.

---

## 5. `test_credential_generation.py`

**Server start:** `QuietServer(('127.0.0.1', 0), RealSessionHandler)` — **real** session logic.
**Patches (started in setUp):**
- `app.CRED_PATH` → tempdir `credentials.json`
- `app.AUTH_DIR` → tempdir
- `app._fails` → `{}`
- `app._global_fails` → `[]`

**Setup:** writes `self.document = {'version':1,'username':'fixture-admin',
'password':app.hash_password('fixture-original-password'),'sessionKey':'12'*32,
'updatedAt':1,'history':[]}` via `app._save_credentials`. Clears `app._fails` and `app._revoked`.
Original pw `fixture-original-password`; rotated pw `fixture-rotated-password`.

**Helper `request(method, path, body=None, token=None)`:** headers
`Content-Type: application/json`, `X-Admin-Request: 1`, and `Cookie: wb2a_admin=<token>`
if token. Returns `(status, dict(headers), headers_list, body)`.

**Helper `rotate()`:** writes updated doc (rotated pw, `sessionKey='34'*32`, `updatedAt=2`)
under `app._cred_lock`.

### T5.1 `test_password_change_cannot_upgrade_an_old_password_login`
- Patches `app.verify_password` to a wrapper that calls the real verify and, **if it
  returns True, immediately rotates credentials** before returning.
- Request: `POST /api/auth/login`, body `{"username":"fixture-admin","password":"fixture-original-password"}` (no token).
- Expected: **status 401** ("a stale password received a session for the new credentials").

### T5.2 `test_concurrent_password_change_cannot_overwrite_new_credentials`
- `token, _ = app.issue_session('fixture-admin')`.
- Same `verify_password` rotate-on-success wrapper.
- Request: `POST /api/auth/password` with token, body
  `{"username":"fixture-admin","current":"fixture-original-password",
  "password":"fixture-late-overwrite","confirm":"fixture-late-overwrite"}`.
- Expected: **status 401** ("a stale password change overwrote the newer credentials").
- State after: `app.verify_password('fixture-rotated-password', app.load_credentials()['password']) is True`
  (the rotated password survived).

### T5.3 `test_sliding_renewal_does_not_survive_password_rotation`
- `token, _ = app.issue_session('fixture-admin', ttl=120)`.
- Patches `app.read_session` to call the real one and, **if payload truthy, rotates**.
- Request: `GET /api/session` with token.
- Expected: **status 200**; at least one `Set-Cookie` whose `wb2a_admin` value is
  non-empty (`renewed` list); and `app.read_session(value) is None` for **every** renewed
  token (renewal must not re-sign a stale session with the new signing key).

### T5.4 `test_login_budget_counts_an_in_progress_attempt`
- Patches `app.LOGIN_MAX_FAILS` → `1`, `app.verify_password` → `delayed_verify` that
  appends to `attempts`; on the first call sets `entered` and waits (3 s) for `release`;
  always returns `False`.
- Body `{"username":"fixture-admin","password":"fixture-wrong-password"}`.
- Thread 1: `POST /api/auth/login` (blocked inside verify). Main thread waits for
  `entered`, then issues a **second** `POST /api/auth/login` with the same body.
- Expected: first request → **401**; second request → **429**; `len(attempts) == 1`
  (the in-progress attempt already consumed the budget, so the second never reaches verify).

---

## 6. `test_account_display.py`

**No HTTP server.** Two test classes.

### `AccountDisplayTests` — file-content assertions (NOT backend boundary; recorded for completeness)
Reads `app.js`, `app.py`, `index.html` as text and asserts literal substrings exist:
- `app.py` contains `"coolRemaining"`, `"coolKind"`, `"reason"`, `"successCount"`, `"errTotal"`.
- `app.js` contains `function coolLeft(sec)`, `function coolReason(p)`,
  `coolLeft(p.coolRemaining)`, `coolReason(p)`, `var coolSub = p.cooling`,
  `esc(coolLeft(p.coolRemaining)`, `'凭证剩 '`, `'无成功记录'`, `'最近错误 '`,
  `function startCoolTicker()`, `function tickCooldowns()`, `data-cool-end=`,
  `startCoolTicker();`, `(ss < 10 ? '0' : '') + ss`, `COOL_RELOAD_PENDING`,
  `if (document.hidden) return;`.
- `index.html` contains `function coolLeft(sec)`, `function coolReason(p)`, `'无成功记录'`,
  `data-cool-end=`.

**Backend-relevant fields** (from `build_state` account `pool` object) that these tests
require the API to expose: `coolRemaining` (int, default 0), `coolKind` (str),
`reason` (str), `successCount` (int), `errTotal` (int), plus `cooling` (bool), `until`,
`disabled`, `disabledReason`, `inFlight`, `breakerFails`, `lastSuccess`, `lastErr`,
`inPool`, `healthy`.

### `RealmCapabilityTest`
**Helper `_state(realm)`:** patches (on `app`):
- `list_auth_files` → `[{"uid":"u1","file":"f.json","disabled":False,"path":"p"}]`
- `auth_summary` → `{"uid":"u1","realm":realm,"nickname":"n","disabled":False,"expiresAt":0,"domain":"","enterpriseId":"","parsable":True}`
- `gateway_get` → `{}`
- `credits_by_uid` → `({}, {"accounts": []})`
- `container_running` → `True`
- `_expiring_soon_env` → `""`

Returns `panel_app.build_state()`.

### T6.1 `test_trae_account_reports_its_own_credits`
- `_state("trae")["accounts"][0]["credits"]["supported"] is True`.

### T6.2 `test_all_realms_report_credits`
- For each `realm in ("cn","global","trae")`: `credits["supported"] is True`.

**Required API contract:** every account object in `build_state()["accounts"]` carries
`credits.supported == True` (hardcoded `True` in current app.py) regardless of realm.

---

## 7. `test_account_storage.py`

**No HTTP server.** Two test classes; auth files under a tempdir.

### `AuthFileListingTests`
**Setup:** `AUTHS_DIR` patched to a temp `auths/` dir. `write(name, doc)` writes JSON.
Doc shapes: `nested(uid)` = `{"auth":{"accessToken":"tok","expiresAt":1893456000,
"realm":"cn","domain":"copilot.tencent.com"},"account":{"uid":uid,"nickname":"Nick "+uid[:4]}}`;
`flat(uid)` = top-level `{"accessToken":"tok","expiresAt":1893456000,"uid":uid,
"nickname":"Flat "+uid[:4],"realm":"cn","domain":"copilot.tencent.com"}`.

### T7.1 `test_hyphenless_files_are_listed`
- Writes `workbuddy-<a×32>.json` (nested), `workbuddy_new.json` (flat), `workbuddy.json` (flat).
- `[e["file"] for e in app.list_auth_files()] ==
  ["workbuddy-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.json","workbuddy.json","workbuddy_new.json"]`
  (sorted lexicographically; all three visible, incl. hyphenless names).

### T7.2 `test_disabled_suffix_is_still_recognized`
- Writes `workbuddy_new.json.disabled`. `list_auth_files()` returns 1 item;
  `items[0]["disabled"] is True`; `items[0]["file"] == "workbuddy_new.json.disabled"`.

### T7.3 `test_non_auth_files_are_ignored`
- `workbuddy-<a×32>.json` + `notes.txt` + `other.json` →
  only `["workbuddy-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.json"]`.

### T7.4 `test_uid_comes_from_content_not_filename`
- `workbuddy_new.json` with flat `uid = "d"*32` → `list_auth_files()[0]["uid"] == "d"*32`.

### T7.5 `test_uid_falls_back_to_filename_when_content_unreadable`
- `workbuddy-<e5×32>.json` containing `"{broken"` → uid `"e5"*16` = `"e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5"`.
- `workbuddy-f0f0f0f0.json` containing `"{broken"` → `app.find_entry("f0f0f0f0")["file"] == "workbuddy-f0f0f0f0.json"`.

### T7.6 `test_flat_form_summary_reads_top_level_fields`
- Flat doc for uid `"f"*32`: `auth_summary(...)` → `uid=="f"*32`, `realm=="cn"`,
  `domain=="copilot.tencent.com"`, `expiresAt==1893456000`, `nickname=="Flat ffff"`.

### T7.7 `test_find_entry_reaches_hyphenless_account`
- `find_entry("b"*32)` returns non-None with `entry["file"] == "workbuddy_new.json"`.

### `AccountStorageTests`
**Setup:** tempdir `root/` with `auths/` and `trash/`; `uid = "a"*32`;
`active = auths/workbuddy-<uid>.json`; `disabled = active + ".disabled"`.
`self.handler = app.Handler.__new__(app.Handler)`. Patches `AUTHS_DIR` and `TRASH_DIR`.
`entry(path, disabled=False)` → `{"uid":uid,"path":str(path),"file":path.name,"disabled":disabled}`.

### T7.8 `test_failed_new_login_keeps_disabled_credential`
- `disabled` contains `"previous credential"`.
- Patches `app.os.replace` → `OSError("injected persistence failure")`, `app.chown_app` → no-op.
- `app.write_auth_file({"uid":uid,"access_token":"fixture-only-token","expires_in":3600})`
  raises `OSError`.
- `disabled.exists()` is True and its content is still exactly `"previous credential"`
  (a failed save must not delete the previous disabled credential).

### T7.9 `test_toggle_conflict_does_not_delete_other_credential`
- `active` = `"active credential"`, `disabled` = `"different disabled credential"`.
- Patches `find_entry` → `entry(active)`, `chown_app` → no-op,
  `restart_container` → `(True,"ok",0)` (assert `not_called`).
- `self.handler.account_toggle({"uid":uid,"disabled":True})` →
  `result["ok"] is False`; both files keep their exact original contents;
  `restart_container` not called.
- Message (app.py): `"存在同名的启用与禁用凭据，已保留两份文件，请先核对重复账号"`.

### T7.10 `test_same_second_deletes_keep_both_recovery_files`
- Patches `find_entry` → `entry(active)`, `app.time.strftime` → `"20260918-120000"`,
  `restart_container` → `(True,"ok",0)`.
- Writes `"first credential"` → `account_delete({"uid":uid})["ok"] is True`;
  then `"second credential"` → same.
- `sorted(p.read_text() for p in trash.iterdir()) == ["first credential","second credential"]`
  (both recovery files survive same-second deletes; filenames must be uniquified).

### T7.11 `test_restart_does_not_report_unhealthy_service_as_success`
- For `health in (None, {"error":{"message":"unavailable"}})`, patches `docker` →
  `(0,"container","")`, `gateway_get` → health, `time.sleep` → no-op,
  `time.monotonic` → `itertools.count()`.
- `app.restart_container()` → `ok is False`.

### T7.12 `test_restart_accepts_a_real_healthy_service_response`
- Patches `docker` → `(0,"container","")`, `gateway_get` → `{"service":"workbuddy2api","healthy":0}`,
  `time.sleep` → no-op.
- `app.restart_container()` → `ok is True`.

**Contract:** `restart_container` success requires `gateway_get("/healthz")` to be a dict
with `service == "workbuddy2api"` and no `error` key; `{"error":...}` or `None` → failure.

---

## 8. `test_ops_management.py`

**Server start:** `setUpClass` → `ThreadingHTTPServer(("127.0.0.1", 0), FixtureHandler)`,
daemon thread. No env vars.

**Helper `request(path, method="GET", body=None, session=True)`:** headers
`Content-Type: application/json`, `X-Admin-Request: 1`, plus `Cookie: fixture-session`
(session=True) or `Authorization: Bearer ordinary-fixture-key` (session=False).
Returns `(status, json.loads(response.read()))`.

### T8.1 `test_ops_are_admin_only`
- For paths `/api/key-limits`, `/api/requests`, `/api/requests/req_fixture` with
  `session=False` (Bearer key, no admin cookie), patching `app.key_management.request`
  (assert `not_called`): status == **401**; backend not called.

### T8.2 `test_queries_forward_only_to_fixed_private_endpoints`
- Patches `key_management.socket_path` → `"fixture.sock"`,
  `key_management.request` → `(200,{"ok":True})` (assert called once).
- Cases (path → expected forwarded target):
  - `/api/key-limits` → `/key-limits`
  - `/api/requests` → `/requests`
  - `/api/requests?model=global%3Ahy3&status=error&offset=0&limit=20`
    → `/requests?model=global%3Ahy3&status=error&offset=0&limit=20`
  - `/api/requests/req_fixture` → `/requests/req_fixture`
- Status == **200**; `request.assert_called_once_with("fixture.sock", "GET", target, None)`.

### T8.3 `test_bad_queries_do_not_touch_private_transport`
- For each of these paths, patching `key_management.request` (assert `not_called`):
  status == **400**:
  - `/api/key-limits?path=/keys` (key-limits takes no params)
  - `/api/requests?key_id=a&key_id=b` (duplicate)
  - `/api/requests?secret=x` (unsupported param)
  - `/api/requests?limit=0`
  - `/api/requests?limit=101`
  - `/api/requests?offset=-1`
  - `/api/requests?status=unknown`
  - `/api/requests?model=%0d%0aX-Test%3Atrue` (control chars)
  - `/api/requests?model=%FF` (invalid UTF-8)
  - `/api/requests/..%2fkeys` (request-id regex fails)
  - `/api/requests/%2Fkeys` (request-id regex fails)
  - `/api/requests/req_fixture?x=y` (request-id route takes no params)

### T8.4 `test_private_errors_keep_their_http_status`
- For `status in (400,404,503)`, patches `socket_path` → `"fixture.sock"`,
  `request` → `(status,{"ok":False,"message":"fixture"})`.
- `GET /api/requests/req_fixture` → response status == the upstream status.

### T8.5 `test_limits_create_update_and_explicit_clear`
- `limits = {"requests_per_minute":60,"max_concurrent":2,"queue_timeout_seconds":5}`.
- Cases (path, body, expected method, expected endpoint, expected forwarded body):
  - `/api/keys`, `{"name":"fixture","limits":limits}`, `POST`, `/keys`, `{"name":"fixture","limits":limits}`
  - `/api/keys/update`, `{"id":"legacy","limits":limits}`, `PATCH`, `/keys/legacy`, `{"limits":limits}`
  - `/api/keys/update`, `{"id":"legacy","limits":None}`, `PATCH`, `/keys/legacy`, `{"limits":None}`
  - `/api/keys/update`, `{"id":"legacy","name":"renamed"}`, `PATCH`, `/keys/legacy`, `{"name":"renamed"}`
- All called as `self.request(path, "POST", body)`; status == **200**;
  `request.assert_called_once_with("fixture.sock", method, endpoint, forwarded)`.
- Note: `id` is stripped from the forwarded update body.

### T8.6 `test_invalid_limit_policy_is_not_silently_coerced`
- For each bad `limits` value, `POST /api/keys/update` with `{"id":"legacy","limits":limits}`
  → status == **400**, `key_management.request` not called:
  `True`, `[]`, `1`, `{"typo":1}`, `{"requests_per_minute":-1}`,
  `{"requests_per_minute":60001}`, `{"max_concurrent":257}`,
  `{"queue_timeout_seconds":31}`, `{"queue_timeout_seconds":1}`,
  `{"requests_per_minute":True}`, `{"max_concurrent":None}`, `{"max_concurrent":"2"}`,
  `{"max_concurrent":2.5}`.
- Message (app.py): `"限流需为整数：每分钟 0–60000 次、并发 0–256、等待 0–30 秒；排队需先设置并发上限"`.
- Rules encoded: int type strictly (bool excluded); ranges `requests_per_minute` 0–60000,
  `max_concurrent` 0–256, `queue_timeout_seconds` 0–30; if `queue_timeout_seconds > 0`
  then `max_concurrent > 0`; `None` is accepted (explicit clear).

---

## 9. `test_update.py`

**Server start:** `setUpClass` → `ThreadingHTTPServer(("127.0.0.1", 0), Handler)`
(`test_update.Handler`, `_session` → `{"u":"test-admin"}` iff `Cookie: test-session`),
daemon thread. No env vars.

**Helper `request(path, body=None, headers=None, method="POST")`:** default headers
`Cookie: test-session`, `Content-Type: application/json`, `X-Admin-Request: 1`;
`headers` override. Returns `(status, json.loads(response.read()))`.

**STATUS fixture object** (returned by mocked upstream) keys: `enabled, current ("1.3.0"),
commit, state, latest_tag, update_ready, asset_name, asset_size, checked_at,
inherited_fd, repo, dir, built_at, last_error`.

### T9.1 `test_status_requires_admin_session`
- Patch `app.key_management.request` (assert `not_called`).
- `GET /api/update` with `Cookie: ""`, `Authorization: Bearer ordinary-api-key`.
- Expected: **401**; upstream not called.

### T9.2 `test_status_proxies_management_socket`
- Patches `socket_path` → `/tmp/test.sock`, `request` → `(200, {"ok":True,"status":STATUS})`
  (assert called once).
- `GET /api/update` → status **200**; `result["status"]["current"] == "1.3.0"`;
  `request.assert_called_once_with("/tmp/test.sock","GET","/update",None)`.

### T9.3 `test_status_reports_unreachable_gateway`
- Patch `socket_path` → `side_effect=OSError("missing")`.
- `GET /api/update` → status **200**; `result["ok"] is False`;
  `"无法读取" in result["message"]`.

### T9.4 `test_apply_requires_session_and_same_origin`
- Patch `key_management.request` (assert `not_called`).
- `POST /api/update/apply` body `"{}"` with `Cookie: ""` → **401**.
- Then with each of `{"Origin":"https://unrelated.invalid"}` and `{"X-Admin-Request":""}`
  → **403**.
- Upstream never called.

### T9.5 `test_apply_forwards_to_management_socket`
- Patches `socket_path` → `/tmp/test.sock`, `request` → `(200,{"ok":True,"status":STATUS})`.
- `POST /api/update/apply` body `"{}"` → status **200**; `result["ok"] is True`;
  `request.assert_called_once_with("/tmp/test.sock","POST","/update/apply",{})`.

### T9.6 `test_apply_accepts_only_tag_field`
- Good: body `{"tag":"v1.3.1"}` → **200**;
  forwarded `{"tag":"v1.3.1"}` via `POST /update/apply`.
- Bad bodies → **400**, upstream not called:
  - `{"tag":"../etc/passwd"}` (tag regex `[A-Za-z0-9._-]*` fails on `/`)
  - `{"tag":"v"*65}` (length > 64)
  - `{"tag":1}` (not a string)
  - `{"force":true}` (unsupported field)

### T9.7 `test_check_forwards_to_management_socket`
- `POST /api/update/check` body `"{}"` → **200**;
  `request.assert_called_once_with("/tmp/test.sock","POST","/update/check",{})`.

### T9.8 `test_same_origin_is_required_for_key_writes_too`
- Patch `key_management.request` (assert `not_called`).
- `POST /api/keys` body `'{"name":"c"}'` with `Origin: http://127.0.0.1:1` → **403**.

---

## 10. `test_usage_integrity.py`

**Frontend JS execution via Node `vm`** — NOT an HTTP/backend test. Runs `usage.js`
in a sandbox with fake `$`, `esc`, `exactTokens`. Asserts on `US.from/US.to`,
`usageModelsFor`, `usageTotalsForScope`, `usageTotalsFor`, `renderUsageFilter`,
`renderUsageTiles`. **Excluded from the backend PARITY CHECKLIST.**

(For reference, the invariants it locks: date-filtered model breakdown cannot be
proportionally invented → `[]`; no daily history → zeros for the selected day;
"全部" button shows all-time total; valid daily totals exact; failed/unreported
counts preserved per day; unknown usage is labeled 失败或中断 / 用量未完整返回 and
`5,120 tokens`; a complete refresh clears the prior incomplete notice.)

---

## 11. `test_security_boundaries.py`

### `ManagementBoundaryTests`
**Server start:** `setUpClass` → `QuietServer(("127.0.0.1",0), SessionHandler)`, daemon thread.
**Helper `request(path, body="{}", headers=None)`:** default headers
`Cookie: fixture-session`, `Content-Type: application/json`, `X-Admin-Request: 1`;
`headers` override. Returns `(status, raw_bytes)` or `("connection_failed", type(error).__name__)`.

### T11.1 `test_all_mutating_routes_require_same_origin_and_management_header`
- Routes (path → patched method name):
  `/api/service/restart`→`service_restart`, `/api/account/delete`→`account_delete`,
  `/api/account/toggle`→`account_toggle`, `/api/task/toggle`→`task_toggle`,
  `/api/task/run`→`task_run`, `/api/features/reasoning-loop`→`reasoning_loop_toggle`,
  `/api/login/start`→`login_start`, `/api/login/poll`→`login_poll`.
- For each route × each header override (`Origin: https://untrusted.invalid`,
  `X-Admin-Request: ""`): patch the handler method (assert `not_called`),
  POST with body `"{}"` → status == **403**.

### T11.2 `test_invalid_json_never_triggers_restart`
- For body in `"not json"`, `"null"`, `"[]"`, `"123"`, `"\"text\""`:
  patch `service_restart` (assert `not_called`);
  `POST /api/service/restart` → **400**.
- (Non-object JSON and unparsable JSON are rejected before the action; note `"null"`
  → parsed as `None` → not a dict → 400.)

### T11.3 `test_body_framing_and_type_are_bounded_before_action`
- Cases (header override → expected status), patching `service_restart` (assert `not_called`):
  - `Content-Length: invalid` → **400**
  - `Content-Length: -1` → **400**
  - `Content-Length: 9999999` → **413**
  - `Content-Type: text/plain` → **415**
- Messages (app.py): 400 `"请求长度无效"`; 413 `"请求体超过大小限制"`;
  415 `"请使用 JSON 格式提交"`. (Note `-1` fails the ASCII-digit regex → 400
  `"请求长度无效"`, not `"请求体不能为空"`.)

### T11.4 `test_toggle_requires_explicit_boolean_and_valid_identifier`
- Cases (route, patched method, payload) all → **400**, method not called:
  - `/api/account/toggle`, `account_toggle`, `{"uid":"a"*32,"disabled":"false"}` (string bool)
  - `/api/account/toggle`, `account_toggle`, `{"uid":"a"*32}` (missing disabled)
  - `/api/task/toggle`, `task_toggle`, `{"key":"checkin","enabled":"false"}` (string bool)
  - `/api/task/toggle`, `task_toggle`, `{"key":[],"enabled":True}` (key not a valid task id)
- Message for boolean: `"开关值必须明确指定为 true 或 false"`; for key: `"任务标识不正确"`.

### T11.5 `test_reasoning_loop_toggle_requires_explicit_boolean`
- Payloads → **400**, method not called: `{}`, `{"stop_only":"true"}`, `{"stop_only":1}`,
  `{"stop_only":None}`, `{"stop_only":True,"extra":1}` (extra field rejected).

### T11.6 `test_reasoning_loop_toggle_reports_gateway_failure`
- Patches `key_management.request` → `(500,{"ok":False,"message":"网关未响应"})`,
  `socket_path` → `/tmp/fixture.sock`.
- `POST /api/features/reasoning-loop` body `{"stop_only":true}` → HTTP **200**;
  `data["ok"] is False`; `"网关未响应" in data["message"]`.

### T11.7 `test_reasoning_loop_toggle_forwards_explicit_value`
- Patches `request` → `(200,{"ok":True,"stop_only":True})`, `socket_path` → `/tmp/fixture.sock`.
- `POST /api/features/reasoning-loop` body `{"stop_only":true}` → HTTP **200**;
  `data["ok"] is True`; `data["stop_only"] is True`;
  `request.call_args[0][3] == {"stop_only": True}` (4th positional arg = body).

### T11.8 `test_logs_keep_stderr_when_stdout_is_present`
- Patch `app.docker` → `(0, "ordinary request output", "WARN: diagnostic on stderr")`.
- `GET /api/logs` with `Cookie: fixture-session`.
- Expected: `data["rows"] == []` (no parseable request rows);
  `"\n".join(str(line) for line in data["other"])` contains both
  `"ordinary request output"` and `"diagnostic on stderr"` (raw text not returned in a
  separate field, but stderr diagnostics preserved in `other`).
- Note: the GET request here sends **no `X-Admin-Request`** header (only Cookie);
  `/api/logs` is a GET and does not require the management header.

### `SessionBoundaryTests`
**Setup:** tempdir; writes `credentials.json` = `{"version":1,"username":"fixture-admin",
"password":hash_password("fixture-password-123"),"sessionKey":"12"*32,"updatedAt":1,"history":[]}`.
Patches `AUTH_DIR`, `CRED_PATH`, `HTPASSWD_PATH` (→ missing), `_revoked` (→ `set()`),
`_fails` (→ `{}`), `_global_fails` (→ `[]`).
Server: `QuietServer(("127.0.0.1",0), RealSessionHandler)`.
**Helper `logout(token)`:** `POST /api/auth/logout` body `"{}"` with
`Cookie: wb2a_admin=<token>`, `Content-Type: application/json`, `X-Admin-Request: 1`;
returns `(status, headers, body)`.

### T11.9 `test_root_deployment_login_cookie_authenticates_next_request`
- Uses `urllib` cookie jar. `POST /api/auth/login` body
  `{"username":"fixture-admin","password":"fixture-password-123"}` with
  `Content-Type: application/json`, `X-Admin-Request: 1`.
- Login response JSON `["ok"] is True`.
- Next `GET /api/session` (cookie carried by jar) → HTTP **200**.

### T11.10 `test_logout_does_not_send_a_fresh_sliding_session`
- `token, _ = issue_session("fixture-admin", ttl=120)`.
- `logout(token)` → status **200**; there is at least one `Set-Cookie`; **all** `Set-Cookie`
  values start with `"wb2a_admin=;"` (i.e. cleared, not a fresh session);
  `read_session(token) is None`.

### T11.11 `test_logout_revocation_survives_process_memory_reset`
- Issue session, logout (200). Then `app._revoked.clear()` (simulate process memory reset).
- `read_session(token) is None` (revocation persisted to `credentials.json` `revokedSessions`).

### T11.12 `test_logout_revokes_copies_from_before_sliding_renewal`
- `original, _ = issue_session("fixture-admin", ttl=120)`.
- `GET /api/session` with `Cookie: wb2a_admin=<original>` → read a non-empty renewed
  `Set-Cookie` value (`renewed`).
- `logout(renewed)` → **200**. Clear `_revoked`.
- `read_session(original) is None` **and** `read_session(renewed) is None`.

### T11.13 `test_corrupt_credentials_are_not_replaced_by_new_login`
- For each of `b'{"username":"fixture-admin",broken'`, `b"null"`, `b"[]"`, `b"{}"`:
  write to `credentials.json`; `app.load_credentials()` raises `(OSError, ValueError)`;
  file bytes unchanged afterward.

### `LoginThrottleBucketTests`
**setUp:** `app._fails.clear()`; cleanup re-clears. Helper `handler(peer, headers)`
returns object with `.headers` and `.client_address = (peer, 1234)`.

### T11.14 `test_untrusted_peer_ignores_forwarded_headers`
- Peer `198.51.100.7` with each of `{"X-Real-IP":"203.0.113.1"}`,
  `{"CF-Connecting-IP":"203.0.113.2"}`, `{"X-Forwarded-For":"203.0.113.3, 10.0.0.1"}`
  → `app._client_ip(handler) == "198.51.100.7"`.

### T11.15 `test_spoofed_header_cannot_reset_the_bucket`
- For `i in range(LOGIN_MAX_FAILS + 3)` (9): `ip = _client_ip(handler("198.51.100.7",
  {"X-Real-IP":"203.0.113.%d"%i}))` then `login_failed(ip)`.
- `login_blocked("198.51.100.7")` → `blocked is True`, `hits == LOGIN_MAX_FAILS + 3` (9);
  `len(app._fails) == 1` (spoofed headers did not create extra buckets).

### T11.16 `test_trusted_peer_honours_forwarded_ip`
- `_client_ip(handler("127.0.0.1",{"X-Real-IP":"203.0.113.9"})) == "203.0.113.9"`.
- `_client_ip(handler("172.17.0.1",{"X-Real-IP":"203.0.113.10"})) == "203.0.113.10"`.
- `_client_ip(handler("127.0.0.1",{"X-Forwarded-For":"1.2.3.4, 10.0.0.1"})) == "1.2.3.4"`
  (first entry, comma-split).

### T11.17 `test_trusted_peer_without_headers_falls_back_to_peer`
- `_client_ip(handler("127.0.0.1",{})) == "127.0.0.1"`.

### T11.18 `test_expired_bucket_is_evicted_not_emptied`
- `login_failed("stale")`; then `app._fails["stale"] = [time.time() - LOGIN_WINDOW - 1]`.
- `login_blocked("stale")` → `blocked is False`, `hits == 0`, `"stale" not in app._fails`
  (empty/expired bucket removed, not left as `[]`).

### T11.19 `test_bucket_table_is_bounded`
- `login_failed("198.51.100.%d" % i)` for `i in range(LOGIN_BUCKET_LIMIT + 2000)`.
- `len(app._fails) <= LOGIN_BUCKET_LIMIT` and `> 0`.

### T11.20 `test_unparsable_peer_is_treated_as_untrusted`
- `_is_trusted_proxy("") is False`; `_is_trusted_proxy("not-an-ip") is False`;
  `_is_trusted_proxy("127.0.0.1") is True`.

---

## PARITY CHECKLIST

Every observable behavior a Go reimplementation must reproduce for the 11 listed test
files (excluding `test_usage_integrity.py`, which is pure frontend JS, and the
`AccountDisplayTests`/`AppDisplay` file-substring assertions, which are frontend source
checks — those are noted at the end as backend-field contracts).

### Request gating & framing (do_POST)
- C1. Mutating POST routes require a valid admin session cookie; with no session → **401**.
  Routes: `/api/service/restart`, `/api/account/delete`, `/api/account/toggle`,
  `/api/task/toggle`, `/api/task/run`, `/api/features/reasoning-loop`, `/api/login/start`,
  `/api/login/poll`, `/api/auth/password`, `/api/update/check`, `/api/update/apply`.
- C2. On those routes, a request whose `Origin` header is present and does not equal the
  request `Host` (scheme `http`/`https`, case-insensitive netloc, no userinfo) → **403**
  with body `{"ok": false, "message": "请求来源无效，请从管理页面重新操作"}`. Absent
  `Origin` is treated as same-origin (allowed).
- C3. On those routes, `X-Admin-Request` must be exactly the string `"1"`; otherwise → **403**
  (same message as C2). Empty/absent header fails.
- C4. All early 401/403 rejections from the gating path and from `/api/keys*` must send a
  response header `Connection: close`, must have a non-empty JSON body, must set the
  connection to close, and must still drain a late request body without emitting a second
  response or a traceback.
- C5. Body framing validation (before the action runs):
  - `Transfer-Encoding` present → **400** `{"ok": false, "message": "不支持此请求传输方式"}`.
  - `Content-Type` (before `;`) not equal (case-insensitive, trimmed) to `application/json`
    → **415** `{"ok": false, "message": "请使用 JSON 格式提交"}`.
  - `Content-Length` absent, duplicated, or not matching `^[0-9]{1,10}$` (ASCII digits only,
    1–10 chars) → **400** `{"ok": false, "message": "请求长度无效"}`. This includes
    `invalid`, `-1`, `+2`, `2.0`, and any non-ASCII/latin-1 digit byte (e.g. `0xB2`).
  - Parsed length `<= 0` → **400** `{"ok": false, "message": "请求体不能为空"}`.
  - Length `> limit` (65536 default; 8192 for `/api/update/*`) → **413**
    `{"ok": false, "message": "请求体超过大小限制"}` (e.g. `Content-Length: 9999999`).
  - Body not complete valid UTF-8 JSON → **400** `{"ok": false, "message": "请求体不是完整的 JSON"}`.
  - Parsed JSON not an object → **400** `{"ok": false, "message": "请求体必须是 JSON 对象"}`
    (covers `not json`, `null`, `[]`, `123`, `"text"`).
- C6. The latin-1 Content-Length byte case must produce a **400** JSON response whose
  `ok` is `false`, and must never produce **500**, a `Traceback`, or
  `invalid literal for int` in the response.
- C7. A valid `Content-Length: 2` + body `{}` to `/api/service/restart` without a session
  must reach the session gate → **401** (i.e. framing success does not itself reject).
- C8. `_reject_unread_body(code, payload)` semantics: set `close_connection`; emit the JSON
  response with `Connection: close`; then, only if `Transfer-Encoding` is absent and there
  is exactly one `Content-Length` matching `^[0-9]{1,10}$`, drain at most `min(len, 65536)`
  bytes in chunks of at most 4096, under a hard 0.1 s wall-clock deadline that per-read
  timeouts must not extend; a single 1000000000 length drains exactly 65536 bytes;
  peer disconnect/timeout/`ValueError` during drain is swallowed and must not alter or
  duplicate the rejection.

### Login / auth
- C9. `POST /api/auth/login` when global login throttle is active → **429**, response header
  `Retry-After: <n>`, body `{"ok": false, "message": "登录尝试过于频繁，请 <n> 秒后再试"}`
  (with `<n>` matching the header). Must be **exactly one** HTTP response on the connection
  (no trailing second response / no 500). With throttle value 42: header `Retry-After: 42`,
  message `登录尝试过于频繁，请 42 秒后再试`.
- C10. `POST /api/auth/login` when per-IP bucket is blocked → **429** with
  `{"ok": false, "message": "失败次数过多，请 5 分钟后再试"}` (LOGIN_WINDOW//60 = 5).
- C11. A wrong-password login consumes the attempt budget **before** verifying, so a second
  concurrent login while the first is still verifying returns **429** (with
  `LOGIN_MAX_FAILS=1`), and the verify function is called only once.
- C12. Successful login requires username to match (constant-time) and password to verify;
  on success it must re-read credentials under lock and, if they changed since load
  (username/password/sessionKey differ), return **401**
  `{"ok": false, "message": "登录凭证已更改，请使用新密码重新登录"}` instead of issuing a session.
- C13. Password change (`POST /api/auth/password`): after verifying the current password,
  re-read credentials under lock; if username/password/sessionKey changed → **401**
  `{"ok": false, "message": "登录凭证已更改，请重新登录后修改"}` and the newer on-disk
  credentials must be preserved (not overwritten).
- C14. Sliding session renewal (`GET /api/session`) must sign renewed tokens with the
  credential set loaded **at that moment**; a token renewed just before a password rotation
  must not validate under the new signing key (`read_session(renewed) is None` after rotation).
- C15. `POST /api/auth/logout` → **200**; emits only clearing `Set-Cookie` headers, every one
  starting with `wb2a_admin=;` (no fresh session), and revokes the presented session nonce.
- C16. Logout revocation must persist to `credentials.json` (`revokedSessions`) so it
  survives a process-memory reset; after clearing in-memory revoked set, the token still
  fails validation.
- C17. Logout must revoke both the originally issued token and any renewed copy issued
  before the logout.
- C18. `POST /api/auth/login` on a root deployment must set a cookie that authenticates a
  subsequent `GET /api/session` (cookie path must work at `/`); login response `ok` is `true`.

### Session token / credentials file
- C19. Session tokens are `base64url(payload) + "." + base64url(HMAC-SHA256(key))`; key is
  `credentials.json` `sessionKey` (hex); payload has `u`, `e` (expiry epoch), `n` (24-hex nonce).
- C20. `read_session` returns `None` for: no token, no `.`, bad signature, non-dict payload,
  `n` not matching `^[0-9a-f]{24}$`, `n` revoked (in-memory or persisted), expired `e`,
  or `u` != current credentials username.
- C21. `load_credentials` raises `(OSError, ValueError)` for a corrupt/unreadable
  `credentials.json` (unparsable JSON, `null`, `[]`, `{}`) and must **not** modify the file.
  A valid doc requires non-empty string `username` and `password`; `sessionKey` must match
  `^[0-9a-fA-F]{64}$`; `revokedSessions` (if present) must be a dict.

### Login throttle internals (module-level, directly tested)
- C22. `_bucket_key(ip)`: IPv6 addresses collapse to their `/64` network string
  (`2001:db8:1:2::/64`); IPv4 returns the address string; unparsable input returned verbatim.
- C23. `_client_ip(handler)`: if the direct peer is not a trusted proxy, return the peer
  address and ignore all forwarded headers. If the peer is trusted, use the first non-empty
  of `X-Real-IP`, `CF-Connecting-IP`, `X-Forwarded-For` (taking the part before the first
  comma, trimmed); fall back to the peer if none present.
- C24. `_is_trusted_proxy(peer)`: `""` → false; unparsable → false; `127.0.0.1` → true
  (default trusted set = loopback + private + link-local ULA ranges listed above).
- C25. `login_failed(ip)` appends a timestamp to the per-`_bucket_key(ip)` bucket and to the
  global fail list; rotating addresses within one IPv6 /64 or spoofing `X-Real-IP` from an
  untrusted peer must all land in **one** bucket.
- C26. `login_blocked(ip)` returns `(blocked, hits)` where `blocked` iff windowed hits
  `>= LOGIN_MAX_FAILS` (6); an expired/empty bucket is **removed** from the table (not left
  as `[]`); `hits` counts only timestamps within `LOGIN_WINDOW` (300 s).
- C27. `login_global_retry_after()`: returns `0` until `>= LOGIN_GLOBAL_MAX_FAILS` (20)
  failures within `LOGIN_GLOBAL_WINDOW` (60 s), then `max(1, int(60 - (now - oldest)) + 1)`.
  It must be a **float-free integer-seconds** countdown. Clearing the global list resets it
  to 0. A successful login clears only the caller's bucket and the global list.
- C28. The bucket table is bounded to `LOGIN_BUCKET_LIMIT` (10000) entries; when exceeded,
  evict the oldest ~1/8 by last-failure time. `len(_fails) <= 10000` always, and `> 0` after
  many inserts.

### Admin allowlist
- C29. `admin_client_allowed(handler)`: with `WB2API_ADMIN_ALLOW_CIDRS` unset/empty → allow
  everyone. When set, allow only clients whose (resolved `_client_ip`) address is in one of
  the configured CIDRs, plus loopback always allowed; invalid CIDR entries are ignored;
  unparsable client address → deny.
- C30. When disallowed, GET/POST entry points return **403**
  `{"ok": false, "message": "当前来源不在管理入口白名单内"}`.

### Account / auth-file management
- C31. `list_auth_files()` enumerates files matching `^workbuddy(?P<uid>.*)\.json(\.disabled)?$`
  in `AUTHS_DIR`, sorted by filename, returning `{uid, file, disabled, path}` for each.
  It must list hyphenless names (`workbuddy_new.json`, `workbuddy.json`) and `.disabled`
  variants, and ignore non-matching files (`notes.txt`, `other.json`).
- C32. The `uid` comes from file **content** (`account.uid` for nested form, top-level `uid`
  for flat form); if the content is unreadable/corrupt, fall back to the filename's uid
  segment with leading `-` stripped (e.g. `workbuddy-f0f0f0f0.json` → `f0f0f0f0`).
- C33. `auth_summary(entry)` reads both nested (`auth`/`account`) and flat forms, yielding
  `uid`, `nickname` (falls back to `account.uid` then `uid[:8]`), `enterpriseId`, `realm`,
  `domain`, `expiresAt` (int), `disabled`, `parsable`.
- C34. `find_entry(uid)` rejects uid not matching `^[A-Za-z0-9_-]{1,80}$` (returns None) and
  otherwise finds the entry across the full (wide-glob) listing, including hyphenless files.
- C35. `write_auth_file(poll_result)`: validate uid against UID_RE (else raise
  `RuntimeError("上游返回的 uid 不合法: %r")`), require a non-empty access_token (else
  `RuntimeError("上游没有返回 access_token")`), write via temp file + fsync + chown + atomic
  `os.replace`; on failure (e.g. injected `os.replace` OSError) it must **not** delete or
  modify an existing `*.disabled` credential.
- C36. `account_toggle({uid, disabled})` when both `X.json` and `X.json.disabled` exist →
  `{"ok": false, "message": "存在同名的启用与禁用凭据，已保留两份文件，请先核对重复账号"}`,
  must not rename either file, and must not restart the container.
- C37. `account_delete({uid})` moves the auth file into `TRASH_DIR` named
  `<file>.<YYYYmmdd-HHMMSS>.<12-hex>`, so two deletes within the same second both survive
  with distinct names.
- C38. `restart_container()` returns success **only** if `docker restart` exits 0 **and**
  `gateway_get("/healthz")` yields a dict with `service == "workbuddy2api"` and no `error`
  key (checked until a 45 s deadline). `None` or `{"error": {...}}` → failure.

### Ops (key-limits / requests) admin-only read channel
- C39. `GET /api/key-limits`, `/api/requests`, `/api/requests/<id>` require an admin session;
  with only `Authorization: Bearer <key>` → **401** and no private transport call.
- C40. These endpoints forward **only** to fixed private endpoints:
  `/api/key-limits` → `/key-limits` (no query allowed);
  `/api/requests` → `/requests` (+ the exact validated query string);
  `/api/requests/<id>` → `/requests/<id>`.
  The private call is `key_management.request("fixture.sock", "GET", target, None)`.
- C41. Query validation → **400** (private transport untouched) for:
  any param on `/api/key-limits`; duplicate `key_id`; unknown params (e.g. `secret`);
  `limit` not in 1..100 or non-numeric; negative `offset`; unknown `status`; a `model`
  value containing control chars (`\r\n`) or invalid UTF-8; a request-id containing `/`
  (encoded `..%2fkeys`, `%2Fkeys`) or extra query on a request-id path.
- C42. Upstream HTTP status from the private channel (400/404/503) is passed through verbatim.
- C43. `POST /api/keys` forwards `{"name","note","models","reasoning_loop_guard",
  "global_fallback_to_cn","expires_at","limits"}` to `POST /keys` (extra fields → 400
  `"包含不支持的字段"`).
- C44. `POST /api/keys/update` requires `id` matching `legacy|key_[0-9a-f]{24}` (else 400
  `"密钥标识不正确"`), strips `id`, and forwards the remaining changes as `PATCH /keys/<id>`.
  `limits: null` is forwarded explicitly (clear). No valid change field → 400
  `"没有有效的修改字段"`.
- C45. `limits` policy validation (reject → 400 with
  `"限流需为整数：每分钟 0–60000 次、并发 0–256、等待 0–30 秒；排队需先设置并发上限"`):
  must be a dict or `None`; only keys `requests_per_minute` (0–60000),
  `max_concurrent` (0–256), `queue_timeout_seconds` (0–30); values must be strictly `int`
  (reject `bool`, `None`, strings, floats); if `queue_timeout_seconds > 0` then
  `max_concurrent > 0`. All of `True`, `[]`, `1`, `{"typo":1}`, `{"requests_per_minute":-1}`,
  `{"requests_per_minute":60001}`, `{"max_concurrent":257}`, `{"queue_timeout_seconds":31}`,
  `{"queue_timeout_seconds":1}`, `{"requests_per_minute":True}`, `{"max_concurrent":None}`,
  `{"max_concurrent":"2"}`, `{"max_concurrent":2.5}` must be rejected.

### Update channel
- C46. `GET /api/update` requires an admin session (Bearer-only → 401, no upstream call);
  otherwise proxies `GET /update` over the management socket, returning
  `{"ok": true, "status": {...}}` verbatim (200), with the call
  `request(sock, "GET", "/update", None)`.
- C47. If the management socket path can't be resolved (`OSError`), `GET /api/update` still
  returns **200** with `{"ok": false, "message": <contains "无法读取">}`.
- C48. `POST /api/update/apply`: no session → **401**; bad `Origin` or missing
  `X-Admin-Request` → **403**; valid → proxies `POST /update/apply` (200, `{"ok": true, ...}`).
- C49. `/api/update/apply` body: only the `tag` field allowed (`{"force":true}` → 400
  `"包含不支持的字段"`); `tag` if present must be a string, length ≤ 64, matching
  `^[A-Za-z0-9._-]*$` (so `../etc/passwd` → 400 `"版本号格式不正确"`, 65 chars → 400, non-string
  → 400). `{}` forwards `{}`; `{"tag":"v1.3.1"}` forwards `{"tag":"v1.3.1"}`.
- C50. `POST /api/update/check` forwards `POST /update/check` with body `{}`.
- C51. Key-write routes (`/api/keys*`) enforce the same same-origin + `X-Admin-Request`
  gate as update routes (`POST /api/keys` with `Origin: http://127.0.0.1:1` → **403**, no
  upstream call).

### Action-body validation (shared `_validate_action_body`)
- C52. Unknown fields for a known route → **400** `{"ok": false, "message": "包含不支持的字段"}`
  (e.g. `{"stop_only":true,"extra":1}` on `/api/features/reasoning-loop`).
- C53. String fields (`username,password,current,confirm,realm,uid,key,callback`) must be
  strings → **400** `"字段格式不正确：<field>"`.
- C54. `/api/account/toggle` `disabled` and `/api/task/toggle` `enabled` and
  `/api/features/reasoning-loop` `stop_only` must be strictly `bool` (`type(x) is bool`) →
  otherwise **400** `"开关值必须明确指定为 true 或 false"` (rejects `"false"`, `1`, `None`,
  and missing).
- C55. `/api/account/*` routes: `uid` must match `^[A-Za-z0-9_-]{1,80}$` → otherwise **400**
  `"账号标识不正确"`.
- C56. `/api/task/run` and `/api/task/toggle`: `key` must be in the task whitelist
  (`checkin, trae_check, travel, activity, keepalive, school, cat, redeem, lottery, makeup`)
  → otherwise **400** `"任务标识不正确"` (also for non-string `key` like `[]`).
- C57. `/api/login/poll` `login_id` must be a 32-hex string → otherwise **400**
  `"登录流程标识无效，请重新生成链接"`.

### Reasoning-loop toggle
- C58. `POST /api/features/reasoning-loop` with valid `{"stop_only": true}` forwards
  `{"stop_only": true}` to the management socket `POST /features/reasoning-loop` and returns
  HTTP **200** `{"ok": true, "stop_only": <bool>, "message": "已切换为「命中即停止」"}` (or
  `"已切换为「命中后自动重发」"` when false).
- C59. If the gateway returns non-200 or `ok != true`, the panel returns HTTP **200** with
  `{"ok": false, "message": <upstream message or "网关未响应（旧版本网关请先升级）">}` —
  never a silent success.

### Logs
- C60. `GET /api/logs` (admin session; GET needs no `X-Admin-Request`) runs
  `docker logs --tail <request_window_size(lines)> <container>`, parses request rows via the
  request-log regex, keeps at most the last `want` rows (default 120, clamped 1..1000),
  and returns `{"ok": <rc==0>, "rows": [...], "other": [...last 60 unparsed lines...],
  "count": len(rows), "rc": rc}`. It must **not** return the raw log text, but unparsed
  lines (including stderr diagnostics) must be preserved in `other`.

### State / credits capability (build_state)
- C61. `build_state()` returns `accounts`, each with a `pool` object exposing
  `coolRemaining`, `coolKind`, `reason`, `successCount`, `errTotal`, `cooling`, `until`,
  `disabled`, `disabledReason`, `inFlight`, `breakerFails`, `lastSuccess`, `lastErr`,
  `inPool`, `healthy`.
- C62. Each account exposes `credits.supported == true` for **every** realm (`cn`, `global`,
  `trae`), plus `remain`, `size`, `used`, `packages`, `ok`, `error`, `expiring`,
  `expiry`, `expiringWindowHours`. Top-level `build_state` also exposes `service`,
  `version`, `panelVersion`, `containerRunning`, `pool`, `creditTotals`, `creditError`,
  `creditPending`, `pendingRestart`, `apiKey` (always `""`), `apiKeysManaged`.

### Frontend-source contract (backend must keep these fields/literals; asserted by
### `test_account_display.py` against source files — relevant only if the Go port also
### serves/replaces app.js/index.html)
- C63. The served `index.html` must contain the cooldown helpers
  (`function coolLeft(sec)`, `function coolReason(p)`, `'无成功记录'`, `data-cool-end=`) and
  the account status/last-activity rendering must use `coolRemaining`/`coolKind`/`reason`/
  `successCount`/`errTotal`, prefer cooldown over credential expiry, and distinguish
  `无成功记录` from `最近错误`.
