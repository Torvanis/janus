package httpapi

import (
	"context"
	"fmt"
	"time"

	"github.com/torvanis/janus/internal/alerting"
	"github.com/torvanis/janus/internal/license"
	"github.com/torvanis/janus/internal/store"
)

// Alert triggers for pools, personal subscriptions, the license and Chat.
// Each is claimed in the database (Store.ClaimAlert) or fired on a
// database state transition, so N replicas send it once.

// chatFailureAlertEvery rate-limits "Chat replies failing" per model.
const chatFailureAlertEvery = 15 * time.Minute

func poolAlertKey(poolID, modelID string) string { return "pool_member_down:" + poolID + ":" + modelID }

// alertPoolMemberDown announces a member a pool took out of rotation, once
// per ejection across the cluster.
func (s *Server) alertPoolMemberDown(ctx context.Context, poolID string, model *store.Model, until time.Time, why string) {
	if s.Alerts == nil || s.Store == nil {
		return
	}
	window := time.Until(until)
	if window < time.Minute {
		window = time.Minute
	}
	if !s.claimAlert(ctx, poolAlertKey(poolID, model.ID), window) {
		return
	}
	pool := poolID
	if mm, err := s.Store.ManagedModelByID(ctx, poolID); err == nil && mm.Name != "" {
		pool = mm.Name
	}
	s.Alerts.Dispatch(ctx, alerting.Event{
		Trigger: alerting.TriggerPoolMemberDown, Severity: "warning", NotifyAdmins: true,
		Title: "Pool " + pool + ": " + model.PublicName() + " taken out of rotation",
		Body: "Requests to " + pool + " go to its other members until " + until.UTC().Format("15:04 MST") +
			", when Janus tries this member again. Reason: " + why,
		Data: map[string]any{"pool_id": poolID, "model_id": model.ID, "until": until.UTC(), "reason": why},
	})
}

// markSubscriptionReauth records that a personal subscription needs
// reconnecting and, on the transition only, tells its owner.
func (s *Server) markSubscriptionReauth(ctx context.Context, conn *store.SubscriptionConnection, providerName, reason string) {
	changed, err := s.Store.MarkSubscriptionReauthOnce(ctx, conn.ID, reason)
	if err != nil || !changed || s.Alerts == nil || conn.UserID == "" {
		return
	}
	s.Alerts.Dispatch(context.WithoutCancel(ctx), alerting.Event{
		Trigger: alerting.TriggerSubscriptionReauth, Severity: "warning", UserID: conn.UserID,
		Title: "Reconnect your " + providerName + " subscription",
		Body:  "Janus can no longer use your " + providerName + " plan: " + reason + " Reconnect it under Subscriptions to keep using its models.",
		Data:  map[string]any{"subscription_id": conn.ID, "provider": conn.Provider},
	})
}

// licenseAlertWindow is how long one license notice holds per status: an
// approaching expiry is repeated weekly, grace and expiry daily.
func licenseAlertWindow(st license.Status) time.Duration {
	if st == license.StatusExpiring {
		return 7 * 24 * time.Hour
	}
	return 24 * time.Hour
}

// OnLicenseState runs after every license refresh on this replica (start-up,
// the hourly re-check, install, remove, sync). It keeps the expiry gauge
// current and alerts admins, once per status and window cluster-wide, when
// the license is expiring, in grace or expired.
func (s *Server) OnLicenseState(st license.State) {
	s.Metrics.SetLicenseExpiry(st.ExpiresAt)
	if s.Alerts == nil || s.Store == nil || st.ExpiresAt == nil {
		return
	}
	var sev, title, body string
	exp := st.ExpiresAt.UTC().Format("2 January 2006")
	switch st.Status {
	case license.StatusExpiring:
		sev, title = "warning", "Janus license expires on "+exp
		body = "Renew it to keep the " + string(st.Edition) + " features. Administration → Settings → License & updates shows the details."
	case license.StatusGrace:
		sev, title = "critical", "Janus license expired on "+exp+" (grace period)"
		body = "Everything keeps working during the grace period. Renew now; after it ends, creating new items is blocked."
	case license.StatusExpired:
		sev, title = "critical", "Janus license has expired"
		body = "The license expired on " + exp + ". Existing configuration keeps working, but creating new items is blocked until a valid license is installed."
	default:
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	key := fmt.Sprintf("license_expiring:%s:%d", st.Status, st.ExpiresAt.Unix())
	if !s.claimAlert(ctx, key, licenseAlertWindow(st.Status)) {
		return
	}
	s.Alerts.Dispatch(ctx, alerting.Event{
		Trigger: alerting.TriggerLicenseExpiring, Severity: sev, NotifyAdmins: true, Title: title, Body: body,
		Data: map[string]any{"status": st.Status, "expires_at": st.ExpiresAt, "edition": st.Edition},
	})
}

// alertChatFailure tells admins Chat replies are failing on a model, at most
// once per chatFailureAlertEvery per model across the cluster.
func (s *Server) alertChatFailure(model, code, msg string) {
	if s.Alerts == nil || s.Store == nil || model == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !s.claimAlert(ctx, "chat_failure:"+model, chatFailureAlertEvery) {
		return
	}
	s.Alerts.Dispatch(ctx, alerting.Event{
		Trigger: alerting.TriggerChatFailure, Severity: "warning", NotifyAdmins: true,
		Title: "Chat replies are failing on " + model,
		Body:  "A Chat reply failed: " + msg + " Further failures on this model are not announced for 15 minutes; the request log shows each one.",
		Data:  map[string]any{"model": model, "error_code": code},
	})
}
