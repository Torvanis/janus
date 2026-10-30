package httpapi

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
)

// Chat's model calls are in-process gateway requests. A chatOrigin carries
// what they need from the browser request that started the reply, so the
// gateway treats them as that person's traffic from where they are:
//   - the client address (and X-Forwarded-For only when it came through a
//     trusted proxy) and the browser's headers and user agent, so IP and
//     header policy blocking rules apply to Chat and the request log shows
//     where Chat traffic came from;
//   - the conversation id, sent as X-Janus-Session, so auto-routing keeps a
//     conversation on one route whatever tools or memories change;
//   - the reply's message id and its first request id: every call carries
//     the message id (the footer sums them), and every call but the first
//     is a sub-call that counts once toward request limits.

const (
	// Request-log names of Chat's calls (X-Janus-Agent): the reply's first
	// call is janus-chat; later rounds of the same reply, deep-research
	// rounds, and media/tool calls name themselves.
	chatToolClientApp     = "janus-chat-tool"
	chatResearchClientApp = "janus-chat-research"
	chatMediaClientApp    = "janus-chat-media"
)

type chatOrigin struct {
	remoteAddr     string
	forwardedFor   string // only when the peer is a trusted proxy
	userAgent      string
	referer        string
	header         http.Header // the browser's headers, for policy rules only
	conversationID string
	messageID      string
	firstRequestID string
	// calls counts the gateway calls issued for the reply (each writes one
	// usage event), so the footer waits for all of them.
	calls atomic.Int64
}

// chatCall is what handleProxy learns about an in-process Chat call.
type chatCall struct {
	origin  *chatOrigin
	subcall bool
}

const (
	ctxChatOrigin ctxKey = "chat_origin"
	ctxChatCall   ctxKey = "chat_call"
)

// chatOriginFor captures the browser request's identity for a reply.
func (s *Server) chatOriginFor(r *http.Request, conversationID, messageID, firstRequestID string) *chatOrigin {
	h := http.Header{}
	for k, v := range r.Header {
		switch strings.ToLower(k) {
		case "cookie", "authorization", "x-janus-csrf", "content-length", "content-type":
			continue
		}
		h[k] = append([]string(nil), v...)
	}
	return &chatOrigin{remoteAddr: r.RemoteAddr, forwardedFor: s.trustedForwardedFor(r), userAgent: r.Header.Get("User-Agent"),
		referer: r.Header.Get("Referer"), header: h, conversationID: conversationID, messageID: messageID, firstRequestID: firstRequestID}
}

func withChatOrigin(ctx context.Context, o *chatOrigin) context.Context {
	if o == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxChatOrigin, o)
}

func chatOriginFrom(ctx context.Context) *chatOrigin {
	o, _ := ctx.Value(ctxChatOrigin).(*chatOrigin)
	return o
}

func chatCallFrom(ctx context.Context) *chatCall {
	c, _ := ctx.Value(ctxChatCall).(*chatCall)
	return c
}

// stampChatOrigin puts the reply's identity on one in-process request.
func stampChatOrigin(req *http.Request, requestID string) *http.Request {
	o := chatOriginFrom(req.Context())
	if o == nil {
		return req
	}
	req.RemoteAddr = o.remoteAddr
	if o.forwardedFor != "" {
		req.Header.Set("X-Forwarded-For", o.forwardedFor)
	}
	if o.conversationID != "" {
		req.Header.Set("X-Janus-Session", o.conversationID)
	}
	o.calls.Add(1)
	call := &chatCall{origin: o, subcall: requestID != o.firstRequestID}
	return req.WithContext(context.WithValue(req.Context(), ctxChatCall, call))
}

// chatAgentFor names a call that is the reply itself when it is the reply's
// first request (an image model's picture), else a sub-call.
func chatAgentFor(ctx context.Context, requestID, sub string) string {
	if o := chatOriginFrom(ctx); o == nil || o.firstRequestID == requestID {
		return chatClientApp
	}
	return sub
}

// chatInsightsSkips reports whether a Chat call is not a request of its own
// for Usage insights: every sub-call of a reply — the auto-title, media
// calls (describe, transcribe, music) and the later tool or research rounds,
// which resend the same message. The person's message is classified once,
// on the reply's first call.
func chatInsightsSkips(c *chatCall) bool {
	return c != nil && c.subcall
}

// chatCallHeaders is what header policy rules see for a Chat call: the
// browser's headers, then the call's own (X-Janus-Agent, Content-Type).
func chatCallHeaders(c *chatCall, own http.Header) http.Header {
	h := c.origin.header.Clone()
	if h == nil {
		h = http.Header{}
	}
	for k, v := range own {
		if strings.EqualFold(k, "User-Agent") {
			continue
		}
		h[k] = v
	}
	return h
}
