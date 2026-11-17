package store

// chatGAMigration turns Janus Chat on once for every install when Chat
// leaves preview (owner ruling: on for everyone after upgrade). Until then
// JANUS_CHAT_PREVIEW was a hard switch above Administration → Chat's
// Enabled, so an install may have saved Enabled=false while Chat could not
// run anyway. Only the top-level "enabled" key of the stored `chat`
// setting changes: the document is always written by json.Marshal of
// ChatAdminSettings, whose first field is Enabled, so the value starts
// with exactly {"enabled":false, when it is off. Nested switches (such as
// search.enabled) and every other setting are left as saved. An install
// that never saved the setting has no row and already defaults to on.
// Forward only: an administrator who wants Chat off turns it off again in
// Administration → Chat, and that choice is never overridden.
var chatGAMigration = migration{name: "0086_chat_generally_available", stmt: []string{
	`UPDATE system_setting SET value = '{"enabled":true,' || substr(value, 18)
	 WHERE key = 'chat' AND value LIKE '{"enabled":false,%'`,
}}
