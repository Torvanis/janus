package httpapi

import (
	"context"
	"fmt"
	"time"

	"github.com/torvanis/janus/internal/alerting"
	"github.com/torvanis/janus/internal/truststore"
)

// CheckTrustStoreExpiry alerts administrators once per certificate when an
// uploaded or mounted certificate is within truststore.ExpiryWarning of its
// expiry, and again once it has expired. The embedded DoD roots are
// refreshed per release and not alerted on. main calls it every hour.
func (s *Server) CheckTrustStoreExpiry(ctx context.Context) {
	if s.Alerts == nil || s.Store == nil {
		return
	}
	s.CheckListenerCertExpiry(ctx)
	for _, c := range truststore.Current().CAs {
		if c.Source == truststore.SourceDoD || (!c.ExpiresSoon && !c.Expired) {
			continue
		}
		state, sev := "expiring", "warning"
		title := "Trusted certificate " + c.Name + " expires on " + c.NotAfter.UTC().Format("2 January 2006")
		if c.Expired {
			state, sev = "expired", "critical"
			title = "Trusted certificate " + c.Name + " has expired"
		}
		if !s.claimAlert(ctx, fmt.Sprintf("trusted_ca:%s:%s", c.SHA256, state), 45*24*time.Hour) {
			continue
		}
		where := "Administration → Settings → Certificates"
		if c.ReadOnly {
			where = "the deployment (" + c.Path + ")"
		}
		s.Alerts.Dispatch(ctx, alerting.Event{
			Trigger: alerting.TriggerSystem, Severity: sev, NotifyAdmins: true, Title: title,
			Body: "Connections to servers whose certificates chain only to this one will fail once it expires. Replace it in " + where + ".",
			Data: map[string]any{"subject": c.Subject, "sha256": c.SHA256, "not_after": c.NotAfter.UTC(), "source": c.Source},
		})
	}
}
