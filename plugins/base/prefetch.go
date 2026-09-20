package alpsbase

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/migadu/alps"
	"github.com/migadu/alps/provider"
)

// triggerInboxPrefetch kicks off a background inbox warm-up for a freshly
// authenticated session, if enabled in the server config. It must be called
// synchronously from an HTTP handler, since it reads ctx-bound user settings
// before handing off to a goroutine — ctx is not valid once the handler
// returns.
func triggerInboxPrefetch(ctx *alps.Context, s *alps.Session) {
	if !ctx.Server.Sessions.PrefetchInboxEnabled() {
		return
	}
	limit := ctx.Server.Sessions.PrefetchInboxLimit()

	settings, err := readSettings(ctx)
	if err != nil {
		ctx.Server.Logger().Debugf("inbox prefetch: skipping, failed to read settings: %v", err)
		return
	}

	enableThreading := false
	if settings.UI.EnableThreading != nil {
		enableThreading = *settings.UI.EnableThreading
	} else if val, ok := s.GetData("hasThreadCapability"); ok {
		if hasCap, ok := val.(bool); ok {
			enableThreading = hasCap
		}
	}

	logger := ctx.Server.Logger()
	go prefetchInbox(s, logger, settings, enableThreading, limit)
}

// prefetchInbox fetches metadata and body for up to limit INBOX messages and
// seeds the session cache under the same keys the normal read paths
// (handleGetMessages / handleGetPart in routes.go) use, so the first real
// request lands on a cache hit instead of an IMAP round trip.
func prefetchInbox(s *alps.Session, logger alps.Logger, settings *Settings, enableThreading bool, limit int) {
	defer func() {
		if r := recover(); r != nil {
			logger.Errorf("panic in inbox prefetch: %v", r)
		}
	}()

	prefetchCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	go func() {
		select {
		case <-s.Closed():
			cancel()
		case <-prefetchCtx.Done():
		}
	}()

	sortOrder := settings.SortOrder
	if sortOrder == "" {
		sortOrder = "desc"
	}

	var msgs []provider.Message
	err := s.DoMailWithContext(prefetchCtx, func(p provider.MailProvider) error {
		var err error
		msgs, _, err = p.ListMessages("INBOX", sortOrder, 0, limit)
		return err
	})
	if err != nil {
		logger.Debugf("inbox prefetch: ListMessages failed: %v", err)
		return
	}

	// Seed the page-0 list cache too, but only when the user's real per-page
	// setting fits inside what we fetched — otherwise a short slice would
	// silently truncate their actual first page rather than just missing the
	// cache and re-fetching correctly.
	if settings.MessagesPerPage > 0 && settings.MessagesPerPage <= len(msgs) {
		page0 := msgs[:settings.MessagesPerPage]
		key := fmt.Sprintf("messages:%s:page%d:perpage%d:query%s:sort%s:criteria%s:thread%t",
			"INBOX", 0, settings.MessagesPerPage, "", sortOrder, settings.MessageSortCriteria, enableThreading)
		s.Cache().Set(key, CachedMessages{Messages: page0, Total: len(msgs)})
	}

	preferHTML := settings.PreferredView != "text"

	count := 0
	for i := range msgs {
		select {
		case <-prefetchCtx.Done():
			logger.Debugf("inbox prefetch: stopped early after %d/%d messages", count, len(msgs))
			return
		default:
		}

		imapMsg := providerMessageToIMAP(msgs[i])
		var partPath []int
		if preferHTML {
			if part := imapMsg.HTMLPart(); part != nil {
				partPath = part.Path
			} else if part := imapMsg.TextPart(); part != nil {
				partPath = part.Path
			}
		} else {
			if part := imapMsg.TextPart(); part != nil {
				partPath = part.Path
			} else if part := imapMsg.HTMLPart(); part != nil {
				partPath = part.Path
			}
		}

		uid := msgs[i].ID
		// GetMessagePartWithData fetches the body without IMAP's Peek option
		// (see provider/imap/provider.go), so the server marks the message
		// \Seen as a side effect — exactly like a real "open message" fetch
		// does. Prefetching must stay invisible to the user, so if the
		// message wasn't already seen, restore that afterward. This is a
		// best-effort correction: a real open landing in the narrow window
		// between the two calls would have its \Seen wrongly cleared again,
		// but alps will just re-set it next time the message is read.
		wasSeen := slices.Contains(msgs[i].Flags, provider.FlagSeen)

		var providerMsg *provider.Message
		var headerData, bodyData []byte
		var restoreErr error
		err := s.DoMailWithContext(prefetchCtx, func(p provider.MailProvider) error {
			var err error
			providerMsg, _, headerData, bodyData, err = p.GetMessagePartWithData("INBOX", uid, partPath)
			if err != nil {
				return err
			}
			if !wasSeen {
				restoreErr = p.SetMessagesFlags("INBOX", []provider.MessageID{uid}, provider.FlagOperation{
					Op:    provider.FlagOpRemove,
					Flags: []provider.Flag{provider.FlagSeen},
				})
			}
			return nil
		})
		if err != nil {
			logger.Debugf("inbox prefetch: skipping message %s: %v", uid.String(), err)
			continue
		}
		if restoreErr != nil {
			logger.Debugf("inbox prefetch: fetched message %s but failed to restore its unseen flag: %v", uid.String(), restoreErr)
		} else if !wasSeen {
			providerMsg.Flags = slices.DeleteFunc(providerMsg.Flags, func(f provider.Flag) bool { return f == provider.FlagSeen })
		}

		key := fmt.Sprintf("message:%s:%s:%v:limit%d", "INBOX", uid.String(), partPath, 0)
		s.Cache().Set(key, CachedMessagePart{
			Message:    providerMsg,
			HeaderData: headerData,
			BodyData:   bodyData,
			Mailbox:    "INBOX",
		})
		count++
	}
	logger.Debugf("inbox prefetch: cached %d/%d INBOX messages", count, len(msgs))
}
