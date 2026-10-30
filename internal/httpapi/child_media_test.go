package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/torvanis/janus/internal/store"
)

// kidsWithMedia is strictKids with the Kids range's media switches set.
func kidsWithMedia(media map[string]bool) []map[string]any {
	profiles := strictKids()
	profiles[1]["media"] = media
	return profiles
}

// Child Safety media switches: an age range's image switch opens
// /v1/images/generations for a covered child (the prompt still judged);
// off (the default) keeps it refused.
func TestChildSafetyMediaSwitchesGateImageGeneration(t *testing.T) {
	h, sess := chatHarness(t)
	m := h.familyModel(t, newFamilyDecider(t))
	assign := []map[string]string{{"scope_type": "user", "profile_id": store.ChildProfileKids, "scope_id": h.user.ID}}
	h.putFamily(t, sess, map[string]any{"enabled": true, "classifier_model_id": m.ID, "profiles": kidsWithMedia(nil), "assignments": assign})

	rec := h.do(http.MethodPost, "/v1/images/generations", map[string]any{"model": "test-model", "prompt": "a cat"})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), familyEndpointNotice[:20]) {
		t.Fatalf("image switch off: want the endpoint notice, got %d %s", rec.Code, rec.Body.String())
	}

	out := h.putFamily(t, sess, map[string]any{"enabled": true, "classifier_model_id": m.ID,
		"profiles": kidsWithMedia(map[string]bool{"image": true}), "assignments": assign})
	// The switch is stored and read back.
	fam, _ := out["family"].(map[string]any)
	var kids map[string]any
	for _, p := range fam["profiles"].([]any) {
		if pm := p.(map[string]any); pm["id"] == store.ChildProfileKids {
			kids = pm
		}
	}
	if media, _ := kids["media"].(map[string]any); media["image"] != true || media["video"] != false {
		t.Fatalf("media switches not stored: %v", kids["media"])
	}
	rec = h.do(http.MethodPost, "/v1/images/generations", map[string]any{"model": "test-model", "prompt": "a cat"})
	if rec.Code == http.StatusForbidden {
		t.Fatalf("image switch on: still refused: %s", rec.Body.String())
	}
	// The prompt is still judged: a safety category blocks it.
	rec = h.do(http.MethodPost, "/v1/images/generations", map[string]any{"model": "test-model", "prompt": "a nerve agent lab"})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), familyBlockedNotice[:20]) {
		t.Fatalf("image prompt not judged: %d %s", rec.Code, rec.Body.String())
	}
	// Other media stay refused.
	rec = h.do(http.MethodPost, "/v1/videos", map[string]any{"model": "test-model", "prompt": "a cat"})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), familyEndpointNotice[:20]) {
		t.Fatalf("video switch off: want the endpoint notice, got %d %s", rec.Code, rec.Body.String())
	}
}

// The Chat sub-model pickers (image, video, transcribe, describe) never
// pick a model the child's age range blocks, and a range whose media
// switch is off gets no models for it at all.
func TestChildSafetyChatPickersSkipBlockedModels(t *testing.T) {
	h, sess := chatHarness(t)
	m := h.familyModel(t, newFamilyDecider(t))
	addImageModel(t, h, "img-a")
	addImageModel(t, h, "img-b")
	addMediaModel(t, h, "qwen3-vl-a", []string{"chat"})
	addMediaModel(t, h, "qwen3-vl-b", []string{"chat"})
	addMediaModel(t, h, "whisper-a", []string{"stt"})
	addMediaModel(t, h, "whisper-b", []string{"stt"})
	ctx := context.Background()
	before := h.server.chatDescribeModel(ctx, h.user, "test-model")
	trBefore := h.server.chatTranscribeModel(ctx, h.user)
	if before == "" || trBefore == "" {
		t.Fatalf("no describer/transcriber before Child Safety: %q %q", before, trBefore)
	}
	profiles := kidsWithMedia(map[string]bool{"image": true, "transcription": true})
	profiles[1]["blocked_models"] = []string{"img-a", before, trBefore}
	assign := []map[string]string{{"scope_type": "user", "profile_id": store.ChildProfileKids, "scope_id": h.user.ID}}
	h.putFamily(t, sess, map[string]any{"enabled": true, "classifier_model_id": m.ID, "profiles": profiles, "assignments": assign})

	if imgs := h.server.chatImageModels(ctx, h.user); slices.Contains(imgs, "img-a") || !slices.Contains(imgs, "img-b") {
		t.Errorf("image models = %v, want img-b only", imgs)
	}
	if d := h.server.chatDescribeModel(ctx, h.user, "test-model"); d == "" || d == before {
		t.Errorf("describe model = %q (blocked %q)", d, before)
	}
	if tr := h.server.chatTranscribeModel(ctx, h.user); tr == "" || tr == trBefore {
		t.Errorf("transcribe model = %q (blocked %q)", tr, trBefore)
	}

	// Pictures switched off: no image models, the image models are hidden
	// from the picker, and the catalog says why.
	profiles = kidsWithMedia(map[string]bool{"transcription": true})
	h.putFamily(t, sess, map[string]any{"enabled": true, "classifier_model_id": m.ID, "profiles": profiles, "assignments": assign})
	if imgs := h.server.chatImageModels(ctx, h.user); len(imgs) != 0 {
		t.Errorf("image models with pictures off = %v", imgs)
	}
	if v := h.server.chatMediaModels(ctx, h.user, store.ChatToolVideo); len(v) != 0 {
		t.Errorf("video models with videos off = %v", v)
	}
	rec := h.doAsSession(sess, http.MethodGet, "/api/v1/chat/catalog", nil)
	var cat struct {
		Hidden        []string `json:"hidden"`
		ChildMediaOff []string `json:"child_media_off"`
		Models        []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	decodeInto(t, rec, &cat)
	for _, mm := range cat.Models {
		if strings.HasPrefix(mm.Name, "img-") {
			t.Errorf("picker offers image model %s with pictures off", mm.Name)
		}
	}
	if fmt.Sprint(cat.ChildMediaOff) != "[image video music]" {
		t.Errorf("child_media_off = %v", cat.ChildMediaOff)
	}
}

// Sound and video for a child: refused at upload when the age range may
// not turn speech into text; when it may, always turned into text first
// (never sent to the model unread, even one that hears).
func TestChildSafetySoundAndVideoAttachments(t *testing.T) {
	h, sess := chatHarness(t)
	m := h.familyModel(t, newFamilyDecider(t))
	f := newFakeMediaUpstream(t, h)
	addMediaModel(t, h, "whisper-test", []string{"stt"})
	if res := chatAdminPut(t, h, sess, func(m map[string]any) {
		m["models"] = map[string]any{"test-model": map[string]any{"audio": "yes", "video": "yes"}}
	}); res.StatusCode != http.StatusOK {
		t.Fatalf("admin = %d", res.StatusCode)
	}
	h.server.invalidateChatAdmin()
	assign := []map[string]string{{"scope_type": "user", "profile_id": store.ChildProfileKids, "scope_id": h.user.ID}}
	h.putFamily(t, sess, map[string]any{"enabled": true, "classifier_model_id": m.ID, "profiles": kidsWithMedia(nil), "assignments": assign})
	rec := chatUpload(t, h, sess, "memo.mp3", testMP3)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), childNoMediaUpload[:30]) {
		t.Fatalf("sound upload with transcription off: %d %s", rec.Code, rec.Body.String())
	}

	h.putFamily(t, sess, map[string]any{"enabled": true, "classifier_model_id": m.ID,
		"profiles": kidsWithMedia(map[string]bool{"transcription": true}), "assignments": assign})
	conv := chatConversationFor(t, h, sess, "test-model")
	song := chatUploadOK(t, h, sess, "memo.mp3", testMP3)
	sent := chatSend(t, h, sess, conv, map[string]any{"content": "Where do we meet?", "attachment_ids": []string{song["id"].(string)}})
	chatEvents(t, h, sess, sent["run_id"].(string), "")
	body := f.lastChatBody(t)
	if strings.Contains(body, "input_audio") || !strings.Contains(body, "old mill") {
		t.Fatalf("child's sound went to the model unread: %.400s", body)
	}
}

// waitNotice waits for an admin notification whose title contains want.
func waitNotice(t *testing.T, h *harness, want string) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		notes, _, _ := h.store.ListNotifications(context.Background(), h.user.ID, false, 50)
		for _, n := range notes {
			if strings.Contains(n.Title, want) {
				return true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// Coverage through a team: the child may not leave it themselves; an
// administrator removing them, or deleting the team, notifies admins; a
// deleted team's Child Safety assignment is removed.
func TestChildSafetyTeamCoverageLoss(t *testing.T) {
	h := newHarness(t)
	sess := adminSession(t, h)
	ctx := context.Background()
	m := h.familyModel(t, newFamilyDecider(t))
	kid, _ := h.otherUser(t, "kidteam", "kidteam@example.com")
	kid2, _ := h.otherUser(t, "kidteam2", "kidteam2@example.com")
	team, err := h.store.CreateTeam(ctx, "Kids", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []*store.User{kid, kid2} {
		if err := h.store.AddTeamMember(ctx, team.ID, u.ID, "member", "manual", ""); err != nil {
			t.Fatal(err)
		}
	}
	h.putFamily(t, sess, map[string]any{"enabled": true, "classifier_model_id": m.ID,
		"assignments": []map[string]string{{"scope_type": "team", "profile_id": store.ChildProfileKids, "scope_id": team.ID}}})

	// The child cannot leave the team that protects them.
	kidSess, err := h.server.Sessions.Create(ctx, kid.ID)
	if err != nil {
		t.Fatal(err)
	}
	rec := h.doAsSession(kidSess, http.MethodDelete, "/api/v1/teams/"+team.ID+"/members/"+kid.ID, nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Child Safety") {
		t.Fatalf("self-leave: want 409 Child Safety, got %d %s", rec.Code, rec.Body.String())
	}
	if covered, _ := h.store.SecgwFamilyCovers(ctx, kid.ID); !covered {
		t.Fatal("the child left Child Safety coverage")
	}

	// An administrator removing them is allowed, and admins are told.
	rec = h.doAsSession(sess, http.MethodDelete, "/api/v1/teams/"+team.ID+"/members/"+kid.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin remove: %d %s", rec.Code, rec.Body.String())
	}
	if !waitNotice(t, h, "Child Safety no longer covers "+kid.Name) {
		t.Error("no coverage-loss notification after removal")
	}

	// Deleting the team notifies and drops its assignment.
	rec = h.doAsSession(sess, http.MethodDelete, "/api/v1/admin/teams/"+team.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete team: %d %s", rec.Code, rec.Body.String())
	}
	if !waitNotice(t, h, "Child Safety no longer covers "+kid2.Name) {
		t.Error("no coverage-loss notification after team delete")
	}
	cfg, err := h.store.SecgwFamily(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range cfg.Assignments {
		if a.ScopeID == team.ID {
			t.Fatalf("a deleted team keeps its Child Safety assignment: %+v", a)
		}
	}
}

// A directory (SCIM) group change that drops a child from the team that
// puts them under Child Safety notifies administrators.
func TestChildSafetySCIMCoverageLoss(t *testing.T) {
	h := newHarness(t)
	sess := adminSession(t, h)
	ctx := context.Background()
	m := h.familyModel(t, newFamilyDecider(t))
	r := chi.NewRouter()
	h.server.mountSCIMRoutes(r)
	_, secret, err := h.store.CreateSCIMToken(ctx, "test", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	w, b := scimRequest(t, r, "POST", "/scim/v2/Users", secret, `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"scimkid@example.com","active":true}`)
	if w.Code != 201 {
		t.Fatalf("create user: %d %v", w.Code, b)
	}
	uid := b["id"].(string)
	w, b = scimRequest(t, r, "POST", "/scim/v2/Groups", secret, `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"displayName":"Kids","members":[{"value":"`+uid+`"}]}`)
	if w.Code != 201 {
		t.Fatalf("create group: %d %v", w.Code, b)
	}
	gid := b["id"].(string)
	team, err := h.store.CreateTeam(ctx, "Kids", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetTeamGroupMappings(ctx, team.ID, []string{gid}); err != nil {
		t.Fatal(err)
	}
	h.putFamily(t, sess, map[string]any{"enabled": true, "classifier_model_id": m.ID,
		"assignments": []map[string]string{{"scope_type": "team", "profile_id": store.ChildProfileKids, "scope_id": team.ID}}})
	kid, err := h.store.UserByID(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if covered, _ := h.store.SecgwFamilyCovers(ctx, kid.ID); !covered {
		t.Fatal("the directory child is not covered through the mapped team")
	}
	w, b = scimRequest(t, r, "PUT", "/scim/v2/Groups/"+gid, secret, `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"displayName":"Kids","members":[]}`)
	if w.Code != 200 {
		t.Fatalf("empty group: %d %v", w.Code, b)
	}
	name := kid.Name
	if name == "" {
		name = kid.Email
	}
	if !waitNotice(t, h, "Child Safety no longer covers "+name) {
		t.Error("no coverage-loss notification after a SCIM group change")
	}
}
