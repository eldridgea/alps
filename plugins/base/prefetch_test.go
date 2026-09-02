package alpsbase

import (
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/migadu/alps"
)

func textPlainBody() imap.BodyStructure {
	return &imap.BodyStructureSinglePart{Type: "text", Subtype: "plain"}
}

func htmlAndTextBody() imap.BodyStructure {
	return &imap.BodyStructureMultiPart{
		Subtype: "alternative",
		Children: []imap.BodyStructure{
			&imap.BodyStructureSinglePart{Type: "text", Subtype: "plain"},
			&imap.BodyStructureSinglePart{Type: "text", Subtype: "html"},
		},
	}
}

func TestPartToPrefetch_NoViewablePart(t *testing.T) {
	// A message with only an attachment (no text/html part) has nothing to
	// prefetch — mirrors what handleGetPart's default-part selection would do.
	msg := newTestMessage(&imap.BodyStructureSinglePart{
		Type: "application", Subtype: "pdf",
		Extended: &imap.BodyStructureSinglePartExt{
			Disposition: &imap.BodyStructureDisposition{Value: "attachment"},
		},
	})
	msg.AlpsUID = "1"

	cache := alps.NewCache(time.Minute)
	if part := partToPrefetch(cache, "INBOX", msg); part != nil {
		t.Fatalf("expected no part to prefetch, got %+v", part)
	}
}

func TestPartToPrefetch_PrefersHTMLOverText(t *testing.T) {
	msg := newTestMessage(htmlAndTextBody())
	msg.AlpsUID = "2"

	cache := alps.NewCache(time.Minute)
	part := partToPrefetch(cache, "INBOX", msg)
	if part == nil {
		t.Fatal("expected a part to prefetch")
	}
	if part.MIMEType != "text/html" {
		t.Errorf("expected the HTML part to be preferred, got %q", part.MIMEType)
	}
}

func TestPartToPrefetch_FallsBackToText(t *testing.T) {
	msg := newTestMessage(textPlainBody())
	msg.AlpsUID = "3"

	cache := alps.NewCache(time.Minute)
	part := partToPrefetch(cache, "INBOX", msg)
	if part == nil {
		t.Fatal("expected a part to prefetch")
	}
	if part.MIMEType != "text/plain" {
		t.Errorf("expected the text part, got %q", part.MIMEType)
	}
}

func TestPartToPrefetch_SkipsWhenBodyAlreadyCached(t *testing.T) {
	msg := newTestMessage(textPlainBody())
	msg.AlpsUID = "4"

	cache := alps.NewCache(time.Minute)
	part := partToPrefetch(cache, "INBOX", msg)
	if part == nil {
		t.Fatal("expected a part to prefetch on first check")
	}

	cache.Set(prefetchCacheKey("INBOX", msg.AlpsUID, part.Path), CachedMessagePart{
		BodyData: []byte("already fetched"),
	})

	if part := partToPrefetch(cache, "INBOX", msg); part != nil {
		t.Errorf("expected nil once the body is already cached, got %+v", part)
	}
}

func TestPartToPrefetch_StillFetchesWhenOnlyMetadataCached(t *testing.T) {
	// handleGetMailbox seeds a metadata-only cache entry (BodyData == nil) for
	// every listed message before prefetch runs; that placeholder must not be
	// mistaken for an already-warm body.
	msg := newTestMessage(textPlainBody())
	msg.AlpsUID = "5"

	cache := alps.NewCache(time.Minute)
	part := partToPrefetch(cache, "INBOX", msg)
	if part == nil {
		t.Fatal("expected a part to prefetch")
	}

	cache.Set(prefetchCacheKey("INBOX", msg.AlpsUID, part.Path), CachedMessagePart{
		BodyData: nil,
	})

	if part := partToPrefetch(cache, "INBOX", msg); part == nil {
		t.Error("expected a part to prefetch when only metadata is cached")
	}
}
