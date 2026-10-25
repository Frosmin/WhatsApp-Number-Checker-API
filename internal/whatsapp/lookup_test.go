package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
	"whatsapp-number-checker/internal/checker"
)

func contactResult(state, query string) *waBinary.Node {
	return &waBinary.Node{Tag: "list", Content: []waBinary.Node{{
		Tag: "user", Attrs: waBinary.Attrs{"jid": "12345@lid"},
		Content: []waBinary.Node{{Tag: "contact", Attrs: waBinary.Attrs{"type": state}, Content: []byte(query)}},
	}}}
}

func TestStrictContactResult(t *testing.T) {
	for _, tc := range []struct {
		name    string
		node    *waBinary.Node
		want    bool
		invalid bool
	}{
		{"registered with LID", contactResult("in", "+59170000000"), true, false},
		{"not registered", contactResult("out", "+59170000000"), false, false},
		{"legacy query suffix", contactResult("out", "59170000000@c.us"), false, false},
		{"nil", nil, false, true},
		{"empty", &waBinary.Node{Tag: "list"}, false, true},
		{"missing type", contactResult("", "+59170000000"), false, true},
		{"unknown type", contactResult("error", "+59170000000"), false, true},
		{"missing query", contactResult("out", ""), false, true},
		{"wrong query", contactResult("out", "+59170000001"), false, true},
		{"wrong tag", &waBinary.Node{Tag: "error"}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseContact(tc.node, "+59170000000")
			if got != tc.want || errors.Is(err, checker.ErrInvalidResponse) != tc.invalid {
				t.Fatalf("got %v, %v", got, err)
			}
		})
	}
	duplicate := contactResult("in", "+59170000000")
	duplicate.Content = append(duplicate.GetChildren(), duplicate.GetChildren()[0])
	if _, err := parseContact(duplicate, "+59170000000"); !errors.Is(err, checker.ErrInvalidResponse) {
		t.Fatal("duplicate result accepted")
	}
	missing := contactResult("out", "+59170000000")
	missing.GetChildren()[0].Content = nil
	if _, err := parseContact(missing, "+59170000000"); !errors.Is(err, checker.ErrInvalidResponse) {
		t.Fatal("missing contact accepted as false")
	}
}

func TestMapProviderErrors(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		err                  error
		unavailable, limited bool
		wait                 time.Duration
	}{
		{"disconnected", fmt.Errorf("wrapped: %w", whatsmeow.ErrNotConnected), true, false, 0},
		{"logged out", whatsmeow.ErrNotLoggedIn, true, false, 0},
		{"lost during IQ", whatsmeow.ErrIQDisconnected, true, false, 0},
		{"rate limit", &whatsmeow.IQError{Code: 429}, false, true, time.Minute},
		{"resource limit", &whatsmeow.IQError{Code: 419}, false, true, time.Minute},
		{"long wait", &whatsmeow.IQError{Code: 429, ErrorNode: &waBinary.Node{Attrs: waBinary.Attrs{"retry_after": "180"}}}, false, true, 180 * time.Second},
		{"integer wait", &whatsmeow.IQError{Code: 429, ErrorNode: &waBinary.Node{Attrs: waBinary.Attrs{"retry-after": uint32(120)}}}, false, true, 120 * time.Second},
		{"overflow wait", &whatsmeow.IQError{Code: 429, ErrorNode: &waBinary.Node{Attrs: waBinary.Attrs{"retry_after": "999999999999999999999999"}}}, false, true, time.Minute},
		{"timeout", context.DeadlineExceeded, false, false, 0},
		{"upstream", errors.New("something failed"), false, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mapped := mapError(tc.err)
			if errors.Is(mapped, checker.ErrUnavailable) != tc.unavailable {
				t.Fatalf("unavailable: %v", mapped)
			}
			var limited *checker.RateLimitError
			if errors.As(mapped, &limited) != tc.limited {
				t.Fatalf("limited: %v", mapped)
			}
			if limited != nil && limited.RetryAfter != tc.wait {
				t.Fatalf("wait = %v", limited.RetryAfter)
			}
			if !tc.unavailable && !tc.limited && !errors.Is(mapped, tc.err) {
				t.Fatal("lost original error")
			}
		})
	}
}

// A disconnect must fail this lookup, not resend it after reconnection.
// Assert on the actual transport request, not a source-code string.
func TestLookupTransportDisablesRetryAndBindsPhone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got, err := lookupContact(ctx, "+59170000000", "unique-request-id", func(gotCtx context.Context, query whatsmeow.DangerousInfoQuery) (*waBinary.Node, error) {
		if gotCtx != ctx || !query.NoRetry {
			t.Fatal("lookup lost its cancellation context or allows automatic retry")
		}
		if query.Namespace != "usync" || query.Type != "get" || query.To != types.ServerJID {
			t.Fatalf("wrong IQ destination: %+v", query)
		}
		content, ok := query.Content.([]waBinary.Node)
		if !ok || len(content) != 1 || content[0].Tag != "usync" {
			t.Fatal("missing usync request")
		}
		usync := content[0]
		if usync.Attrs["sid"] != "unique-request-id" || usync.Attrs["context"] != "interactive" || usync.Attrs["mode"] != "query" {
			t.Fatalf("wrong usync attributes: %v", usync.Attrs)
		}
		contactQuery := usync.GetChildByTag("query", "contact")
		if contactQuery.Tag != "contact" || contactQuery.Attrs["addressing_mode"] != "lid" {
			t.Fatal("wrong contact-discovery query")
		}
		contact := usync.GetChildByTag("list", "user", "contact")
		if contact.Content != "+59170000000@c.us" {
			t.Fatalf("request is for another phone: %v", contact.Content)
		}
		return &waBinary.Node{Tag: "iq", Content: []waBinary.Node{{Tag: "usync", Content: []waBinary.Node{*contactResult("out", "+59170000000")}}}}, nil
	})
	if err != nil || got {
		t.Fatalf("expected explicit negative: %v %v", got, err)
	}
}

func TestLookupTransportFailureCannotBecomeFalse(t *testing.T) {
	for _, tc := range []struct {
		name string
		node *waBinary.Node
		err  error
	}{
		{"missing list", &waBinary.Node{Tag: "iq"}, nil},
		{"disconnect", nil, whatsmeow.ErrIQDisconnected},
		{"deadline", nil, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := lookupContact(context.Background(), "+59170000000", "id", func(context.Context, whatsmeow.DangerousInfoQuery) (*waBinary.Node, error) { return tc.node, tc.err })
			if err == nil {
				t.Fatal("transport failure returned a successful negative")
			}
		})
	}
}
