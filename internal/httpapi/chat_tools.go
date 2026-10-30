package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/torvanis/janus/internal/adapter"
	"github.com/torvanis/janus/internal/store"
	"github.com/torvanis/janus/internal/subscription"
	"github.com/torvanis/janus/internal/websearch"
)

// Chat tools (user ruling 2026-10-20: on by default, can be disabled).
//
// v1 offers the provider-run search tools: the provider (OpenAI, xAI)
// searches on its own servers and cites its sources, the way their own
// apps do. A tool is offered only when the chat's model is served by a
// provider that runs it; the admin can turn a tool off for the instance
// (Administration → Chat) and a person for one chat (chat settings).
// What a reply searched and cited is kept, encrypted, on the message.

// chatToolInfo is one tool as the picker and the admin page describe it.
type chatToolInfo struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

var chatToolCatalog = []chatToolInfo{
	{ID: store.ChatToolWebSearch, Label: "Web search", Description: "Searches the web for current information and cites its sources."},
	{ID: store.ChatToolXSearch, Label: "X search", Description: "Searches posts on X for news and what people are saying (xAI models)."},
	{ID: store.ChatToolWebFetch, Label: "Read web pages", Description: "Opens a link you give it and reads the page or PDF. Public web pages only."},
	{ID: store.ChatToolCode, Label: "Run code", Description: "Writes and runs code in the provider's own sandbox for calculations and data. Nothing runs on Janus."},
	{ID: store.ChatToolImage, Label: "Create images", Description: "Makes pictures when you ask for one, with your plan's image model or the organization's."},
	{ID: store.ChatToolFile, Label: "Create files", Description: "Hands you whole files (code, web pages, games, documents, data) to preview and download, instead of pasting them."},
	{ID: store.ChatToolVideo, Label: "Create videos", Description: "Makes short videos when you ask for one, with the organization's video model."},
	{ID: store.ChatToolMusic, Label: "Create music", Description: "Makes music clips when you ask for one, with the organization's music model."},
	{ID: store.ChatToolMemory, Label: "Memory", Description: "Remembers what you ask it to keep across chats. See and delete memories in Settings → Memory."},
}

// chatModelTools is what a model's provider can run (nil = none), from the
// upstream's base URL, or the personal plan's provider.
func (s *Server) chatModelTools(ctx context.Context, model string) map[string]bool {
	if subscription.IsPersonalModel(model) {
		provider, _, ok := subscription.ParseModel(model)
		if !ok {
			return nil
		}
		if p, found := subscription.Get(provider); found {
			return adapter.HostedToolSupport(p.InferenceBaseURL())
		}
		return nil
	}
	resolved, err := s.Store.ResolveModelForRequest(ctx, model)
	if err != nil || resolved.Model == nil || resolved.Managed != nil {
		// Managed models route across members; their providers differ.
		return nil
	}
	up, err := s.cachedUpstreamByID(ctx, resolved.Model.UpstreamID)
	if err != nil || up == nil {
		return nil
	}
	runs := adapter.HostedToolsFor(up.AdapterType, up.BaseURL)
	if up.AdapterType == "gemini" {
		runs = geminiToolsGate(resolved.Model.Name, runs)
	}
	if up.AdapterType == "anthropic" {
		// Claude's search/fetch/code tools: Claude 4.5 and newer.
		if n := strings.ToLower(resolved.Model.Name); strings.Contains(n, "claude-3") || strings.Contains(n, "claude-2") {
			return nil
		}
	}
	return runs
}

// chatToolsFor is the tools a run sends: what the model's provider runs,
// minus what the admin and this chat turned off, in catalog order.
func (s *Server) chatToolsFor(ctx context.Context, c *store.ChatConversation, model string) []string {
	runs := map[string]bool{}
	for t := range s.chatModelTools(ctx, model) {
		runs[t] = true
	}
	// Any chat model may call create_image (Janus runs it); a model that
	// takes no function tools just answers without it.
	runs[store.ChatToolImage] = true
	// A model whose provider runs no search gets Janus-run search when the
	// administrator set it up (off by default).
	if !runs[store.ChatToolWebSearch] && s.chatAdmin(ctx).Search.Ready() {
		runs[store.ChatToolWebSearch] = true
	}
	adminOff := s.chatAdmin(ctx).ToolsOff
	var out []string
	for _, t := range store.ChatTools {
		if runs[t] && !slices.Contains(adminOff, t) && (c == nil || !slices.Contains(c.ToolsOff, t)) {
			out = append(out, t)
		}
	}
	return out
}

// geminiToolsGate: Google runs its built-in tools on Gemini 3 and newer
// only (older models are billed per prompt and refuse them on the native
// path we use); gemini-2.x models get none.
func geminiToolsGate(model string, runs map[string]bool) map[string]bool {
	name := strings.ToLower(model)
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if strings.HasPrefix(name, "gemini-1") || strings.HasPrefix(name, "gemini-2") || strings.HasPrefix(name, "gemma") {
		return nil
	}
	return runs
}

// chatToolsBody is the tools array for a chat completion.
func chatToolsBody(tools []string) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{"type": t})
	}
	return out
}

// chatExtras is what a reply used besides its text: searches and sources.
type chatExtras struct {
	mu       sync.Mutex
	Activity []chatActivity `json:"activity,omitempty"`
	Sources  []chatSource   `json:"sources,omitempty"`
}

type chatActivity struct {
	Type    string   `json:"type"`
	Status  string   `json:"status"`
	Query   string   `json:"query,omitempty"`
	Sources []string `json:"sources,omitempty"`
	// Code and Output: a provider-run program and what it printed.
	Code   string `json:"code,omitempty"`
	Output string `json:"output,omitempty"`
	// Size is a file the reply made (create_file), in bytes.
	Size int64 `json:"size,omitempty"`
}

type chatSource struct {
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
	Host  string `json:"host,omitempty"`
}

// addActivity records a search starting ("searching") or finishing
// ("done", which updates the oldest unfinished one of that type).
func (e *chatExtras) addActivity(a chatActivity) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if a.Status == "searching" && a.Query != "" {
		// The same call, now with its query (streams send it late).
		for i := len(e.Activity) - 1; i >= 0; i-- {
			if e.Activity[i].Type == a.Type && e.Activity[i].Status == "searching" && e.Activity[i].Query == "" {
				e.Activity[i].Query = a.Query
				if a.Code != "" {
					e.Activity[i].Code = a.Code
				}
				return
			}
		}
	}
	if a.Status == "done" {
		for i := range e.Activity {
			if e.Activity[i].Type == a.Type && e.Activity[i].Status == "searching" {
				if a.Query == "" {
					a.Query = e.Activity[i].Query
				}
				if a.Code == "" {
					a.Code = e.Activity[i].Code
				}
				e.Activity[i] = a
				return
			}
		}
	}
	if len(e.Activity) < 64 {
		e.Activity = append(e.Activity, a)
	}
}

// addSource records a cited URL once, in citation order.
func (e *chatExtras) addSource(rawURL, title string) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range e.Sources {
		if s.URL == rawURL {
			return
		}
	}
	if len(e.Sources) >= 50 {
		return
	}
	// Some providers number citations ("1", "2") instead of titling them.
	if strings.Trim(title, "0123456789[] ") == "" {
		title = ""
	}
	e.Sources = append(e.Sources, chatSource{URL: rawURL, Title: title, Host: strings.TrimPrefix(u.Hostname(), "www.")})
}

// snapshot returns a copy safe to marshal while the run continues.
func (e *chatExtras) snapshot() *chatExtras {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.Activity) == 0 && len(e.Sources) == 0 {
		return nil
	}
	return &chatExtras{Activity: slices.Clone(e.Activity), Sources: slices.Clone(e.Sources)}
}

// feed reads the Janus extensions off one chat chunk delta.
func (e *chatExtras) feed(activity json.RawMessage, annotations json.RawMessage) {
	if len(activity) > 0 && string(activity) != "null" {
		var a chatActivity
		if json.Unmarshal(activity, &a) == nil && a.Type != "" {
			e.addActivity(a)
		}
	}
	if len(annotations) > 0 && string(annotations) != "null" {
		var anns []struct {
			Type        string `json:"type"`
			URLCitation struct {
				URL   string `json:"url"`
				Title string `json:"title"`
			} `json:"url_citation"`
		}
		if json.Unmarshal(annotations, &anns) == nil {
			for _, a := range anns {
				if a.Type == "url_citation" {
					e.addSource(a.URLCitation.URL, a.URLCitation.Title)
				}
			}
		}
	}
}

// chatEncodeExtras encrypts the extras for the message row ("" = none).
func (s *Server) chatEncodeExtras(e *chatExtras) string {
	snap := e.snapshot()
	if snap == nil {
		return ""
	}
	b, err := json.Marshal(snap)
	if err != nil {
		return ""
	}
	enc, err := s.chatEncrypt(string(b))
	if err != nil {
		return ""
	}
	return enc
}

// chatDecodeExtras reads a message's stored extras (nil = none).
func (s *Server) chatDecodeExtras(enc string) *chatExtras {
	if enc == "" {
		return nil
	}
	var e chatExtras
	if json.Unmarshal([]byte(s.chatDecrypt(enc)), &e) != nil {
		return nil
	}
	return &e
}

// chatToolsNote tells the model which search tools it has in this chat, so
// it uses them for anything current instead of answering "I have no live
// data" (chats without tools get no note: their prompt is unchanged).
func chatToolsNote(tools []string) string {
	var names []string
	for _, t := range tools {
		switch t {
		case store.ChatToolWebSearch:
			names = append(names, "web search")
		case store.ChatToolXSearch:
			names = append(names, "X (Twitter) search")
		case store.ChatToolWebFetch:
			names = append(names, "web page reading")
		case store.ChatToolCode:
			names = append(names, "code execution in a sandbox")
		}
	}
	if len(names) == 0 {
		return ""
	}
	if slices.Contains(tools, store.ChatToolWebFetch) {
		names[len(names)-1] += " (when the user gives a link, open and read it before answering about it)"
	}
	return "You can use " + strings.Join(names, " and ") + " in this chat. Use them for anything current (news, prices, weather, today's date, recent events) and cite the sources you used."
}

// personalModelNames is the caller's own plan models (my/<provider>/<model>).
func (s *Server) personalModelNames(ctx context.Context, user *store.User) ([]string, error) {
	list, err := s.personalCatalog(ctx, user)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list))
	for _, m := range list {
		if id, _ := m["id"].(string); id != "" {
			out = append(out, id)
		}
	}
	return out, nil
}

// --- create_image: the chat model makes pictures --------------------------

// chatImageToolDef is the function tool offered to a chat model that can
// call tools. Janus runs the call itself through the chat's image model.
var chatImageToolDef = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name":        store.ChatToolImage,
		"description": "Create an image from a detailed description. Use it whenever the user asks for a picture, drawing, photo, logo, illustration or other image. The image is shown to the user; afterwards reply briefly about it.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompt": map[string]any{"type": "string", "description": "A detailed visual description of the image to create."},
			},
			"required": []string{"prompt"},
		},
	},
}

// chatImageModels is every image model this person may use in Chat: their
// own plan's image models first (paid by the plan), then granted ones.
func (s *Server) chatImageModels(ctx context.Context, user *store.User) []string {
	var out []string
	// A child whose age range may not make pictures has no image models.
	child := s.childProfileFor(ctx, user)
	if child != nil && !familyMediaAllowed(child, familyMediaImage) {
		return nil
	}
	if s.chatAdmin(ctx).PersonalPlans {
		if names, err := s.personalModelNames(ctx, user); err == nil {
			for _, n := range names {
				if chatPlanImageModel(n) && !child.Blocks(n) {
					out = append(out, n)
				}
			}
		}
	}
	if catalog, err := s.grantedCatalog(ctx, user, nil); err == nil {
		s.childFilterCatalog(ctx, user, &catalog, nil)
		for _, m := range catalog.Models {
			if name := m.PublicName(); s.chatImageModel(ctx, name) && !s.chatAdmin(ctx).Models[name].Hidden {
				out = append(out, name)
			}
		}
	}
	return out
}

// chatImageModelFor picks the image model for a chat (user ruling: a plan
// chat uses the plan's own image model at $0; otherwise the org's, billed
// to "Working under"). Order: the chat's own choice, the plan's image
// model when the chat runs on that plan, the admin's default, the first
// granted one. "" = none available.
func (s *Server) chatImageModelFor(ctx context.Context, user *store.User, c *store.ChatConversation) string {
	avail := s.chatImageModels(ctx, user)
	if len(avail) == 0 {
		return ""
	}
	if c != nil && c.ImageModel != "" && slices.Contains(avail, c.ImageModel) {
		return c.ImageModel
	}
	// The person's own choice (chat tray → Models), when still available.
	if mine := s.chatUserModel(ctx, user.ID, store.ChatToolImage); mine != "" && slices.Contains(avail, mine) {
		return mine
	}
	if c != nil {
		if provider, _, ok := subscription.ParseModel(c.ModelName); ok {
			for _, m := range avail {
				if p, _, ok := subscription.ParseModel(m); ok && p == provider {
					return m
				}
			}
		}
	}
	if d := s.chatAdmin(ctx).ImageModel; d != "" && slices.Contains(avail, d) {
		return d
	}
	for _, m := range avail {
		if !subscription.IsPersonalModel(m) {
			return m
		}
	}
	return avail[0]
}

// chatImageCallPrompt reads the prompt out of a create_image call.
func chatImageCallPrompt(args string) string {
	var a struct {
		Prompt string `json:"prompt"`
	}
	if json.Unmarshal([]byte(args), &a) != nil {
		return ""
	}
	return strings.TrimSpace(a.Prompt)
}

// chatImageCall makes the image one create_image call asked for, stores
// it on the reply, records it as activity, and returns the tool result for
// the model's follow-up request. made counts images stored so far (at most
// 4 calls are run per reply).
func (s *Server) chatImageCall(ctx context.Context, run *store.ChatRun, live *chatLiveRun, user *store.User, token *store.Token,
	imageModel string, c chatToolCall, made *int) string {
	prompt := chatImageCallPrompt(c.Arguments)
	live.extras.addActivity(chatActivity{Type: store.ChatToolImage, Status: "searching", Query: prompt})
	var result string
	switch {
	case prompt == "":
		result = "No image was made: the call had no prompt."
	case ctx.Err() != nil:
		result = "Stopped by the user."
	case *made >= 4:
		result = "No image was made: a reply can make at most four images."
	default:
		out := s.chatGenerateImage(ctx, run, live, user, token, imageModel, prompt, store.NewID(), *made+1)
		switch {
		case out.stored > 0:
			*made += out.stored
			result = fmt.Sprintf("Created %d image(s) with %s; they are shown to the user above your reply.", out.stored, imageModel)
			if out.caption != "" {
				result += " The image model described it as: " + out.caption
			}
		case out.status == store.ChatStatusBlocked:
			result = "The image was not made: a safety policy blocked it. Tell the user plainly."
		default:
			result = "The image could not be made: " + firstNonEmpty(out.errMsg, "the image model failed") + ". Tell the user."
		}
	}
	live.extras.addActivity(chatActivity{Type: store.ChatToolImage, Status: "done", Query: prompt})
	return result
}

// madeSomething reports a reply that made images, files, video or music
// with Janus's tools: that is an answer even without text.
func (r *chatLiveRun) madeSomething() bool {
	r.extras.mu.Lock()
	defer r.extras.mu.Unlock()
	for _, a := range r.extras.Activity {
		switch a.Type {
		case store.ChatToolImage, store.ChatToolFile, store.ChatToolVideo, store.ChatToolMusic:
			return true
		}
	}
	return false
}

// --- Janus-run web search (models whose provider has none) --------------

// chatSearchToolDef is the function tool offered when Janus runs the search.
var chatSearchToolDef = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name":        "web_search",
		"description": "Search the web for current information (news, prices, weather, today's date, recent events). Returns titles, links and snippets; cite the links you use.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "What to search for."},
			},
			"required": []string{"query"},
		},
	},
}

// chatJanusSearch reports a chat whose web_search Janus runs (the model's
// provider does not search itself).
func (s *Server) chatJanusSearch(ctx context.Context, model string, tools []string) bool {
	if !slices.Contains(tools, store.ChatToolWebSearch) {
		return false
	}
	return !s.chatModelTools(ctx, model)[store.ChatToolWebSearch]
}

// chatRunSearch runs one Janus web search for the chat model.
func (s *Server) chatRunSearch(ctx context.Context, live *chatLiveRun, query string) string {
	cfg := s.chatAdmin(ctx).Search
	live.extras.addActivity(chatActivity{Type: store.ChatToolWebSearch, Status: "searching", Query: query})
	key := ""
	if enc := cfg.Keys[cfg.Provider]; enc != "" {
		key = s.chatDecrypt(enc)
	}
	results, err := websearch.Search(ctx, s.upstreamHTTPClient(ctx), websearch.Config{Provider: cfg.Provider, URL: cfg.URL, Key: key, MaxResults: cfg.MaxResults}, query)
	done := chatActivity{Type: store.ChatToolWebSearch, Status: "done", Query: query}
	if err != nil {
		s.Logger.Warn("chat web search failed", "provider", cfg.Provider, "error", err.Error())
		live.extras.addActivity(done)
		return "The search failed: " + err.Error() + ". Tell the user the search did not work; do not invent results."
	}
	for _, r := range results {
		done.Sources = append(done.Sources, r.URL)
		live.extras.addSource(r.URL, r.Title)
	}
	live.extras.addActivity(done)
	return websearch.FormatForModel(query, results)
}

// chatMaxSearchRounds bounds Janus-run search rounds per reply.
const chatMaxSearchRounds = 4

// chatSearchLimitNote ends the last round's search results.
const chatSearchLimitNote = "\n\n[That was the last web search for this reply. Answer the question now from the results above; do not search again. Say what you could not confirm.]"

func chatSearchCallQuery(args string) string {
	var a struct {
		Query string `json:"query"`
	}
	_ = json.Unmarshal([]byte(args), &a)
	return strings.TrimSpace(a.Query)
}

// handleAdminChatSearchTest runs one search with the settings in the body
// (a key typed but not yet saved is used; otherwise the stored key), so an
// administrator can check a service before turning it on.
func (s *Server) handleAdminChatSearchTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider string `json:"provider"`
		URL      string `json:"url"`
		Key      string `json:"key"`
		Query    string `json:"query"`
	}
	if err := decodeJSON(r, &body); err != nil {
		WriteError(w, r, err)
		return
	}
	cfg := store.ChatSearchSettings{Provider: body.Provider, URL: body.URL, MaxResults: 5}
	if err := cfg.Check(); err != nil {
		writeValidation(w, r, err)
		return
	}
	key := strings.TrimSpace(body.Key)
	if key == "" {
		saved, err := s.Store.ChatAdminSettings(r.Context())
		if err != nil {
			WriteError(w, r, err)
			return
		}
		if enc := saved.Search.Keys[strings.ToLower(strings.TrimSpace(body.Provider))]; enc != "" {
			key = s.chatDecrypt(enc)
		}
	}
	q := strings.TrimSpace(body.Query)
	if q == "" {
		q = "today's top news"
	}
	start := time.Now()
	results, err := websearch.Search(r.Context(), s.upstreamHTTPClient(r.Context()), websearch.Config{
		Provider: strings.ToLower(strings.TrimSpace(body.Provider)), URL: strings.TrimRight(strings.TrimSpace(body.URL), "/"), Key: key, MaxResults: 5}, q)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, websearch.ErrNotConfigured) {
			msg = "Add the API key (or the SearXNG address) first."
		}
		WriteJSON(w, http.StatusOK, map[string]any{"ok": false, "error": msg})
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results, "ms": time.Since(start).Milliseconds()})
}
