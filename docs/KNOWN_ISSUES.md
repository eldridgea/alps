# Known Issues

Pre-existing bugs found incidentally while building other features. Neither
is caused by or specific to that other work — both reproduce on a clean
checkout of `main` — they're recorded here because they aren't otherwise
obvious from the code or git history.

---

## 1. Intermittent deadlock in session teardown / cache cleanup

**Symptom:** `go test .` (the root `alps` package) occasionally hangs until
the test binary's timeout kills it. On timeout, the goroutine dump shows
several goroutines blocked the same way:

```
goroutine N [sync.Mutex.Lock]:
github.com/migadu/alps.(*cacheCleanupManager).unregister(...)
	/home/eldridgea/Code/alps/cache.go:96
github.com/migadu/alps.(*Cache).Close(...)
	/home/eldridgea/Code/alps/cache.go:169
github.com/migadu/alps.(*Session).teardown.func1()
	/home/eldridgea/Code/alps/session.go:377
sync.(*Once).doSlow(...)
github.com/migadu/alps.(*Session).teardown(...)
	/home/eldridgea/Code/alps/session.go:360
github.com/migadu/alps.(*SessionManager).Put.func1()
	/home/eldridgea/Code/alps/session.go:1015
created by github.com/migadu/alps.(*SessionManager).Put
	/home/eldridgea/Code/alps/session.go:971
```

That's the per-session idle-timeout goroutine (`go func() { ... }`, started
inside `Put()` at `session.go:971`) reaching its natural exit path — timer
fires or `s.closed` closes, then it calls `s.teardown()` (`session.go:359`),
which calls `s.cache.Close()` (`session.go:377`) →
`cacheCleanupManager.unregister()` (`cache.go:95`), which needs
`globalCleanupManager.mu.Lock()`. Multiple of these pile up waiting on that
same process-wide mutex at once and, at least sometimes, never all drain.

**Reproduction:** run the root package's tests repeatedly:

```
go test . -timeout 60s -count=1
```

Observed failure rate was roughly 1 in 8–12 runs. Confirmed present on an
unmodified `main` checkout (via `git stash` before and after unrelated
changes), so it predates and is unrelated to whatever else was being worked
on when it was found.

**Not yet root-caused.** Candidates worth checking:
- Whether `cacheCleanupManager.cleanupLoop`'s ticker goroutine
  (`cache.go`, `startGlobalCleanup`/`cleanupLoop`) can end up holding or
  contending `mu` for longer than the brief `RLock`/copy/`RUnlock` it's
  meant to be, e.g. under GC pause or scheduler pressure during a test run
  that creates/expires many short-lived sessions back to back.
- Whether it's a true permanent deadlock or just an very slow drain that
  looks like one at a 60s timeout — a longer timeout plus periodic
  goroutine-count sampling would distinguish the two.
- Whether `-race` catches a real data race feeding into this (not yet run
  with `-race` against a reproduction).

**Impact so far:** observed as test flakiness only. Not yet confirmed to
affect a running server's request-serving path (the blocked goroutines are
all on the session-teardown side), but should be treated as a real
correctness bug until root-caused, since a stuck teardown goroutine also
means that session's provider connection and cache are never released.

---

## 2. Maildir provider can't fetch a genuinely non-multipart message's body

**Symptom:** opening a message that is plain single-part MIME (no
`multipart/*` boundary at all — e.g. a bare `Content-Type: text/plain` body,
or no `Content-Type` header) fails with HTTP 500 from
`handleGetPart` (`plugins/base/routes.go`, ~line 1001 on), logged server-side
as `entity is not multipart`. A genuinely multipart message (e.g.
`multipart/alternative` with text and HTML parts) opens fine.

**Root cause (diagnosed):** per IMAP convention, a non-multipart message's
own body is addressed as part path `[1]`, not `[]` — see how
`IMAPMessage.TextPart()`/`HTMLPart()` (`plugins/base/types.go:81-142`) walk
`BodyStructure` and assign paths; this matches the IMAP provider's own
behavior and the IMAP spec. The maildir provider's part-lookup helper
(feeding `GetMessagePart`/`GetMessagePartWithData`/`GetMessagePartRaw` in
`provider/maildir/messages.go`) instead requires the top-level entity to
itself be multipart before it will address any indexed part, and errors out
for path `[1]` on a non-multipart top-level entity instead of treating that
case as "return the entity itself."

**Reproduction:** any maildir-backed account with a message lacking a MIME
multipart boundary. `plugins/base/http_test.go`'s own shared test fixture
(the two seeded messages, subjects "Engines" and "Looms") are exactly such
bare messages — opening either one via
`GET /mailboxes/INBOX/messages/{uid}` returns 500 today. (Because of this,
newer maildir-provider tests that need a message to actually open use a
real `multipart/alternative` fixture instead — see
`plugins/base/prefetch_test.go`'s `newPrefetchTestSession` for the pattern.)

**Likely fix location:** `provider/maildir/messages.go`'s part-lookup
helper (around `getMessagePartEntity`) needs to special-case "non-multipart
entity + requested path `[1]` (or `[]`)" as "return the top-level entity,"
matching what the IMAP provider and the IMAP spec both already assume.

**Impact:** any maildir deployment where users receive ordinary single-part
plain-text or HTML email — a common case, not just a testing artifact —
cannot open those messages in the UI.
