package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/torvanis/janus/internal/store"
)

func classificationRequest(t *testing.T, h *harness, u *store.User, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	// Route tests are isolated from the parent's migration registration work.
	if _, err := h.store.DB().ExecContext(context.Background(), `CREATE TABLE IF NOT EXISTS report_model_classification (model_id TEXT PRIMARY KEY, family TEXT NOT NULL, provider TEXT NOT NULL, hosting TEXT NOT NULL, updated_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if u != nil {
		req = req.WithContext(context.WithValue(req.Context(), ctxUser, u))
	}
	rr := httptest.NewRecorder()
	router := chi.NewRouter()
	h.server.mountReportClassificationRoutes(router)
	router.ServeHTTP(rr, req)
	return rr
}

func TestReportingClassificationAdminLifecycle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.store.UpdateUser(ctx, h.user.ID, store.RoleAdmin, true); err != nil {
		t.Fatal(err)
	}
	path := "/reports/classifications/" + h.model.ID
	rr := classificationRequest(t, h, h.user, "GET", "/reports/classifications", "")
	if rr.Code != 200 {
		t.Fatalf("list: %d %s", rr.Code, rr.Body)
	}
	var list struct {
		Classifications []store.ReportModelClassification `json:"classifications"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Classifications) != 1 || list.Classifications[0].ModelID != h.model.ID || list.Classifications[0].Hosting != "" {
		t.Fatalf("unknown: %+v", list)
	}
	rr = classificationRequest(t, h, h.user, "PUT", path, `{"family":"Explicit family","provider":"Explicit provider","hosting":"hybrid"}`)
	if rr.Code != 200 {
		t.Fatalf("save: %d %s", rr.Code, rr.Body)
	}
	var saved struct {
		Classification store.ReportModelClassification `json:"classification"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Classification.Family != "Explicit family" || saved.Classification.Hosting != "hybrid" || saved.Classification.UpdatedAt.IsZero() {
		t.Fatalf("saved: %+v", saved)
	}
	got, err := h.store.GetReportModelClassification(ctx, h.model.ID)
	if err != nil || got.Provider != "Explicit provider" {
		t.Fatalf("persistence: %+v %v", got, err)
	}
	rr = classificationRequest(t, h, h.user, "PUT", path, `{"family":"","provider":"","hosting":""}`)
	if rr.Code != 200 {
		t.Fatalf("clear: %d %s", rr.Code, rr.Body)
	}
	got, err = h.store.GetReportModelClassification(ctx, h.model.ID)
	if err != nil || got.Family != "" || got.Provider != "" || got.Hosting != "" {
		t.Fatalf("clear persistence: %+v %v", got, err)
	}
	for _, body := range []string{`{"hosting":"cloud"}`, `{"family":"bad\nvalue"}`, `{"provider":12}`, `{"unexpected":true}`, `null`} {
		rr = classificationRequest(t, h, h.user, "PUT", path, body)
		if rr.Code != 400 {
			t.Errorf("invalid %s: %d %s", body, rr.Code, rr.Body)
		}
	}
	rr = classificationRequest(t, h, h.user, "PUT", "/reports/classifications/missing", `{}`)
	if rr.Code != 404 {
		t.Fatalf("missing: %d %s", rr.Code, rr.Body)
	}
}

func TestReportingClassificationRefreshesAdminAuthorization(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	cached := *h.user
	cached.Role = store.RoleAdmin
	if err := h.store.UpdateUser(ctx, h.user.ID, store.RoleUser, true); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "PUT"} {
		path := "/reports/classifications"
		if method == "PUT" {
			path += "/" + h.model.ID
		}
		rr := classificationRequest(t, h, &cached, method, path, `{}`)
		if rr.Code != 403 {
			t.Fatalf("stale admin %s: %d %s", method, rr.Code, rr.Body)
		}
		rr = classificationRequest(t, h, nil, method, path, `{}`)
		if rr.Code != 401 {
			t.Fatalf("anonymous %s: %d", method, rr.Code)
		}
	}
	if err := h.store.UpdateUser(ctx, h.user.ID, store.RoleAdmin, false); err != nil {
		t.Fatal(err)
	}
	rr := classificationRequest(t, h, &cached, "GET", "/reports/classifications", "")
	if rr.Code != 403 {
		t.Fatalf("inactive admin: %d", rr.Code)
	}
}
