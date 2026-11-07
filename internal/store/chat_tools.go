package store

import (
	"slices"
	"strings"
)

// Chat tools: capabilities a chat model can use while answering. Tools are
// ON by default (user ruling); an administrator can turn one off for the
// instance, a person for one chat. A tool is offered to a model only when
// the model's provider actually runs it (web search: OpenAI and xAI; X
// search: xAI) — Janus never pretends.
var chatToolsMigration = migration{name: "0063_chat_tools", stmt: []string{
	`ALTER TABLE chat_message ADD COLUMN extras_enc TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE chat_conversation ADD COLUMN tools_off TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE chat_conversation ADD COLUMN image_model TEXT NOT NULL DEFAULT ''`,
}, down: []string{
	`ALTER TABLE chat_conversation DROP COLUMN image_model`,
	`ALTER TABLE chat_conversation DROP COLUMN tools_off`,
	`ALTER TABLE chat_message DROP COLUMN extras_enc`,
}}

// Chat tool ids.
const (
	ChatToolWebSearch = "web_search"
	ChatToolXSearch   = "x_search"
	// ChatToolWebFetch reads a page the user names (Claude web fetch,
	// Gemini URL context).
	ChatToolWebFetch = "web_fetch"
	// ChatToolCode runs code in the PROVIDER's sandbox (never on Janus).
	ChatToolCode = "code_execution"
	// ChatToolImage lets the chat model make images: Janus runs the
	// chat's image model when the model calls it.
	ChatToolImage = "create_image"
	// Janus-run tools any chat model that can call functions may use:
	// files it hands over, videos and music made with the chat's media
	// models, and the person's memory (remember / forget).
	ChatToolFile   = "create_file"
	ChatToolVideo  = "create_video"
	ChatToolMusic  = "create_music"
	ChatToolMemory = "memory"
	// ChatToolAsk lets the model stop and ask the person a question with
	// choices (ask_user); the answer is their next message.
	ChatToolAsk = "ask_user"
)

// ChatTools is every tool Chat knows, in display order.
var ChatTools = []string{ChatToolWebSearch, ChatToolXSearch, ChatToolWebFetch, ChatToolCode, ChatToolImage,
	ChatToolFile, ChatToolVideo, ChatToolMusic, ChatToolMemory, ChatToolAsk}

// ValidChatTool reports a known tool id.
func ValidChatTool(id string) bool { return slices.Contains(ChatTools, id) }

// splitToolList parses a stored comma list, dropping unknown ids.
func splitToolList(v string) []string {
	out := []string{}
	for _, id := range strings.Split(v, ",") {
		if id = strings.TrimSpace(id); ValidChatTool(id) && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// CleanChatTools normalizes a requested list (known ids, no duplicates).
func CleanChatTools(ids []string) []string { return splitToolList(strings.Join(ids, ",")) }
