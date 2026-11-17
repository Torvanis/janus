package secgw

import (
	"github.com/torvanis/janus/internal/store"
)

// A request inside a batch file runs later, at the provider, without
// passing through the gateway again. It is inspected when the batch is
// created, with what can be decided then.

// InspectDeferred runs the deterministic checks over a request that will
// run later. stop names the check that refuses it: a blocking match, or a
// redact match (the file is the provider's by now and cannot be rewritten,
// so what would have been removed must not be sent). Observe matches are
// returned to be recorded.
func (e *Engine) InspectDeferred(eff *Effective, body []byte) (vs []Violation, stop store.SecgwCheckKind) {
	if eff.Empty() {
		return nil, ""
	}
	for _, m := range readRequest(body).msgs {
		found := e.scanCached(eff, store.SecgwDirectionIngress, m.Role, m.Text)
		for i := range found {
			found[i].MessageIndex = -1
			found[i].Record = true
			if m.Tool {
				found[i].Direction = store.SecgwDirectionToolOutput
			}
			switch eff.Checks[found[i].Kind].Mode {
			case store.SecgwModeBlock, store.SecgwModeRedact:
				found[i].Action = store.SecgwActionBlocked
				if stop == "" {
					stop = found[i].Kind
				}
			default:
				found[i].Action = store.SecgwActionObserved
			}
		}
		vs = append(vs, found...)
	}
	return vs, stop
}

// BlockingModelCheck names a model-backed check that can stop a request
// (prompt injection or content safety in block mode). Such a check judges
// each request as it is made; one that will run later cannot be judged.
func (e *Effective) BlockingModelCheck() (store.SecgwCheckKind, bool) {
	if e.Empty() {
		return "", false
	}
	for _, kind := range []store.SecgwCheckKind{store.SecgwCheckPromptInjection, store.SecgwCheckContentSafety} {
		if c, ok := e.Has(kind, store.SecgwDirectionIngress); ok && c.Mode == store.SecgwModeBlock {
			return kind, true
		}
	}
	return "", false
}
