package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
	"whatsapp-number-checker/internal/checker"
)

func (c *Client) Check(ctx context.Context, phone string) (bool, error) {
	if !c.Ready() {
		return false, checker.ErrUnavailable
	}
	// IsOnWhatsApp collapses every contact type other than "in" to false.
	// Use its contact-discovery query but inspect the raw "in"/"out" value:
	// missing or future types must be an error, never a false negative.
	// This internal API is deliberately isolated and the module version pinned.
	internal := c.client.DangerousInternals()
	return lookupContact(ctx, phone, internal.GenerateRequestID(), internal.SendIQ)
}

func lookupContact(ctx context.Context, phone, requestID string, send func(context.Context, whatsmeow.DangerousInfoQuery) (*waBinary.Node, error)) (bool, error) {
	// Usync itself enables an automatic resend after disconnection. Build the
	// same request here so the caller's deadline and no-retry policy both hold.
	resp, err := send(ctx, whatsmeow.DangerousInfoQuery{
		Namespace: "usync", Type: "get", To: types.ServerJID, NoRetry: true,
		Content: []waBinary.Node{{
			Tag: "usync", Attrs: waBinary.Attrs{
				"sid": requestID, "mode": "query", "last": "true", "index": "0", "context": "interactive",
			},
			Content: []waBinary.Node{
				{Tag: "query", Content: []waBinary.Node{{Tag: "contact", Attrs: waBinary.Attrs{"addressing_mode": "lid"}}}},
				{Tag: "list", Content: []waBinary.Node{{Tag: "user", Content: []waBinary.Node{{
					Tag: "contact", Content: types.NewJID(phone, types.LegacyUserServer).String(),
				}}}}},
			},
		}},
	})
	if err != nil {
		return false, mapError(err)
	}
	if resp == nil {
		return false, checker.ErrInvalidResponse
	}
	list, ok := resp.GetOptionalChildByTag("usync", "list")
	if !ok {
		return false, checker.ErrInvalidResponse
	}
	return parseContact(&list, phone)
}

func parseContact(list *waBinary.Node, phone string) (bool, error) {
	if list == nil || list.Tag != "list" {
		return false, checker.ErrInvalidResponse
	}
	users := list.GetChildren()
	if len(users) != 1 || users[0].Tag != "user" {
		return false, checker.ErrInvalidResponse
	}
	contacts := users[0].GetChildrenByTag("contact")
	if len(contacts) != 1 || len(users[0].GetChildrenByTag("error")) > 0 {
		return false, checker.ErrInvalidResponse
	}
	contact := contacts[0]
	query, ok := contact.Content.([]byte)
	if !ok || strings.TrimPrefix(strings.TrimSuffix(string(query), "@c.us"), "+") != strings.TrimPrefix(phone, "+") {
		return false, checker.ErrInvalidResponse
	}
	switch contact.Attrs["type"] {
	case "in":
		return true, nil
	case "out":
		return false, nil
	default:
		return false, checker.ErrInvalidResponse
	}
}

func mapError(err error) error {
	var iq *whatsmeow.IQError
	if errors.As(err, &iq) && (iq.Code == 429 || iq.Code == 419) {
		wait := time.Minute
		for _, node := range []*waBinary.Node{iq.ErrorNode, iq.RawNode} {
			if node == nil {
				continue
			}
			for _, attr := range []string{"retry_after", "retry-after"} {
				seconds, parseErr := strconv.ParseUint(fmt.Sprint(node.Attrs[attr]), 10, 64)
				if parseErr == nil && seconds <= uint64((1<<63-1)/time.Second) {
					wait = max(wait, time.Duration(seconds)*time.Second)
				}
			}
		}
		return &checker.RateLimitError{RetryAfter: wait}
	}
	var disconnected *whatsmeow.DisconnectedError
	if errors.Is(err, whatsmeow.ErrNotConnected) || errors.Is(err, whatsmeow.ErrNotLoggedIn) || errors.As(err, &disconnected) {
		return checker.ErrUnavailable
	}
	return err
}
