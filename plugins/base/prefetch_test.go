package alpsbase

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/fernet/fernet-go"
	"github.com/migadu/alps"
	"github.com/migadu/alps/provider"
	"github.com/migadu/alps/provider/maildir"
)

// newPrefetchTestSession spins up a maildir-backed server seeded with n
// genuinely multipart/alternative (text+html) messages in INBOX, and logs
// the test account in.
//
// The messages must be real multipart MIME, not a bare "From:...\r\n\r\nbody"
// blob (which is what newTestServer's own fixture uses): the maildir
// provider's part-fetch code can't yet address a non-multipart message's
// implicit part "1" (it errors "entity is not multipart"), a pre-existing
// bug in provider/maildir hit equally by the real handleGetPart route, not
// something introduced by prefetching. Multipart messages route through a
// different, working code path, so they're what these tests use to exercise
// prefetchInbox's own logic in isolation.
func newPrefetchTestSession(t *testing.T, n int) *alps.Session {
	t.Helper()
	base := t.TempDir()
	passwd := filepath.Join(base, "passwd")
	if err := os.WriteFile(passwd, []byte(testUser+":{PLAIN}"+testPassword+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	store := maildir.NewProvider(filepath.Join(base, "ada"), testUser)
	if err := store.CreateMailbox("INBOX"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		subject := fmt.Sprintf("Subject %d", i)
		msg := "From: Charles <charles@remote.test>\r\n" +
			"To: " + testUser + "\r\n" +
			"Subject: " + subject + "\r\n" +
			"Date: Mon, 02 Jan 2006 15:04:05 +0000\r\n" +
			fmt.Sprintf("Message-ID: <msg%d@remote.test>\r\n", i) +
			"Content-Type: multipart/alternative; boundary=\"BOUNDARY\"\r\n" +
			"\r\n" +
			"--BOUNDARY\r\n" +
			"Content-Type: text/plain; charset=utf-8\r\n" +
			"\r\n" +
			"plain body " + subject + "\r\n" +
			"--BOUNDARY\r\n" +
			"Content-Type: text/html; charset=utf-8\r\n" +
			"\r\n" +
			"<p>html body " + subject + "</p>\r\n" +
			"--BOUNDARY--\r\n"
		if _, _, _, err := store.AppendMessage("INBOX", rawMessage(msg), 0); err != nil {
			t.Fatal(err)
		}
	}

	var key fernet.Key
	if err := key.Generate(); err != nil {
		t.Fatal(err)
	}
	opts := &alps.Options{
		Provider: alps.ProviderOptions{
			Type:    "maildir",
			IMAP:    alps.IMAPProviderOptions{Server: "imap://127.0.0.1:1"},
			Maildir: alps.MaildirProviderOptions{Path: filepath.Join(base, "%u"), AuthPasswdFile: passwd},
		},
		SMTP:         alps.SMTPOptions{Server: "smtp://127.0.0.1:1"},
		LoginKey:     &key,
		CacheEnabled: true,
	}
	srv, err := alps.New(alps.NewLogger(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)

	session, err := srv.Sessions.Put(testUser, testPassword)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(session.Close)
	return session
}

func TestPrefetchInbox_CachesMessagesAndListPage(t *testing.T) {
	session := newPrefetchTestSession(t, 2)

	settings := &Settings{
		MessagesPerPage:     2,
		PreferredView:       "html",
		SortOrder:           "desc",
		MessageSortCriteria: "date",
	}

	prefetchInbox(session, alps.NewDevelopmentLogger(), settings, false, 100)

	key := fmt.Sprintf("messages:%s:page%d:perpage%d:query%s:sort%s:criteria%s:thread%t",
		"INBOX", 0, settings.MessagesPerPage, "", settings.SortOrder, settings.MessageSortCriteria, false)
	cached, ok := session.Cache().Get(key)
	if !ok {
		t.Fatalf("list page cache key %q not populated", key)
	}
	page := cached.(CachedMessages)
	if len(page.Messages) != 2 || page.Total != 2 {
		t.Fatalf("cached page = %+v, want 2 messages, total 2", page)
	}

	for _, msg := range page.Messages {
		imapMsg := providerMessageToIMAP(msg)
		var partPath []int
		if part := imapMsg.HTMLPart(); part != nil {
			partPath = part.Path
		} else if part := imapMsg.TextPart(); part != nil {
			partPath = part.Path
		}
		msgKey := fmt.Sprintf("message:%s:%s:%v:limit%d", "INBOX", msg.ID.String(), partPath, 0)
		mc, ok := session.Cache().Get(msgKey)
		if !ok {
			t.Fatalf("message cache key %q not populated for %s", msgKey, msg.ID.String())
		}
		part := mc.(CachedMessagePart)
		if part.HeaderData == nil || part.BodyData == nil {
			t.Fatalf("message %s cached without header/body data: %+v", msg.ID.String(), part)
		}
	}
}

func TestPrefetchInbox_SkipsListSeedWhenPerPageExceedsFetched(t *testing.T) {
	session := newPrefetchTestSession(t, 2)

	settings := &Settings{
		MessagesPerPage:     50, // the real per-page setting, larger than the capped fetch below
		PreferredView:       "html",
		SortOrder:           "desc",
		MessageSortCriteria: "date",
	}

	// Cap the prefetch itself at 1 message, fewer than MessagesPerPage.
	prefetchInbox(session, alps.NewDevelopmentLogger(), settings, false, 1)

	key := fmt.Sprintf("messages:%s:page%d:perpage%d:query%s:sort%s:criteria%s:thread%t",
		"INBOX", 0, settings.MessagesPerPage, "", settings.SortOrder, settings.MessageSortCriteria, false)
	if _, ok := session.Cache().Get(key); ok {
		t.Fatalf("list page cache key %q should not be seeded when the prefetch limit is smaller than MessagesPerPage", key)
	}
}

func TestPrefetchInbox_DoesNotLeaveMessagesMarkedSeen(t *testing.T) {
	session := newPrefetchTestSession(t, 2)

	settings := &Settings{
		MessagesPerPage:     2,
		PreferredView:       "html",
		SortOrder:           "desc",
		MessageSortCriteria: "date",
	}

	prefetchInbox(session, alps.NewDevelopmentLogger(), settings, false, 100)

	err := session.DoMail(func(p provider.MailProvider) error {
		msgs, _, err := p.ListMessages("INBOX", "desc", 0, 10)
		if err != nil {
			return err
		}
		for _, msg := range msgs {
			for _, f := range msg.Flags {
				if f == provider.FlagSeen {
					t.Errorf("message %s was marked \\Seen by prefetching its body", msg.ID.String())
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
