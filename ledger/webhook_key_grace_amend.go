package ledger

import (
	"context"
	"errors"
	"time"
)

// Outbound webhook signing-key grace-period amendment (LG-335):
// SetWebhookKeyGracePeriod revises one registered endpoint's retired-key
// grace window in place — no Unregister/Register teardown, no delivery
// window in which events are lost. Tightening the grace makes a retired
// key stop verifying immediately (a suspected leak can be stemmed at
// once); widening it keeps a retired key verifiable a while longer (a
// receiver lagging on rotation stays able to verify). A revision only
// changes later Verify/VerifyWithKey expiry judgments and purge timing:
// a delivery already in flight keeps the signature it took at entry,
// Sign always uses the primary, and a key already purged is never
// revived by widening the grace (purge is deletion).

// ErrInvalidWebhookKeyGracePeriod is returned by SetGrace /
// SetKeyGracePeriod / SetWebhookKeyGracePeriod for a negative grace
// period. A runtime revision never silently normalizes: 0 is a legal
// strict value ("a retired key stops verifying immediately"), while a
// negative grace has no meaning.
var ErrInvalidWebhookKeyGracePeriod = errors.New("ledger: webhook: key grace period must not be negative")

// Grace reports the ring's live key grace period: how long after
// RetiredAt a retired key still verifies.
func (k *RotatingKey) Grace() time.Duration {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.grace
}

// SetGrace revises the ring's key grace period in place and returns the
// previous grace. A negative grace returns
// ErrInvalidWebhookKeyGracePeriod with the live grace untouched. A grace
// of 0 is allowed — retired keys stop verifying immediately. A revision
// identical to the live grace is a no-op returning the live grace. The
// revision is future-only: it changes later Verify/VerifyWithKey expiry
// judgments and purge timing, not keys already purged (widening never
// revives a deleted key) and not signatures already taken.
func (k *RotatingKey) SetGrace(d time.Duration) (time.Duration, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if d < 0 {
		return k.grace, ErrInvalidWebhookKeyGracePeriod
	}
	previous := k.grace
	if d == previous {
		return previous, nil
	}
	k.grace = d
	return previous, nil
}

// SetKeyGracePeriod revises one registered endpoint's signing-key grace
// period in place — see RotatingKey.SetGrace. It returns the previous
// grace. An unknown endpoint returns ErrWebhookEndpointNotFound; a
// negative grace returns ErrInvalidWebhookKeyGracePeriod with the live
// grace untouched; a stopped dispatcher refuses the revision (along
// Register's stopped gate). The endpoint's delivery worker is not
// restarted: the queue stays continuous and no event is lost. Lock
// order is d.mu -> k.mu (the ring's own lock); Sign/Verify only ever
// take k.mu and Notify only takes d.mu, so no cycle is introduced.
func (d *WebhookDispatcher) SetKeyGracePeriod(name string, grace time.Duration) (time.Duration, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped.Load() {
		return 0, errors.New("ledger: webhook: dispatcher is stopped")
	}
	ep, ok := d.endpoints[name]
	if !ok {
		return 0, ErrWebhookEndpointNotFound
	}
	return ep.cfg.Keys.SetGrace(grace)
}

// KeyGracePeriod reports one registered endpoint's live signing-key
// grace period. It returns false when the name is unknown.
func (d *WebhookDispatcher) KeyGracePeriod(name string) (time.Duration, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	ep, ok := d.endpoints[name]
	if !ok {
		return 0, false
	}
	return ep.cfg.Keys.Grace(), true
}

// SetWebhookKeyGracePeriod revises one registered endpoint's signing-key
// grace period in place — see SetKeyGracePeriod. It is audited as
// webhook_key_grace_period_retuned (the endpoint plus
// previous_grace_period_ns / current_grace_period_ns, durations in
// nanoseconds). A ledger with no dispatcher attached returns
// ErrWebhooksNotConfigured; an unknown endpoint returns
// ErrWebhookEndpointNotFound; a negative grace returns
// ErrInvalidWebhookKeyGracePeriod with the live grace untouched. A
// revision identical to the live grace is a no-op with no audit event.
// On error the returned duration is the live grace (0 when the endpoint
// does not exist or no dispatcher is attached). The revision bumps
// neither the version nor the audit chain version, writes no journal
// entry, and emits no outbox event. Restarting restores the grace the
// endpoint was registered with.
func (l *Ledger) SetWebhookKeyGracePeriod(name string, d time.Duration) (time.Duration, error) {
	return l.SetWebhookKeyGracePeriodCtx(context.Background(), name, d)
}

// SetWebhookKeyGracePeriodCtx is SetWebhookKeyGracePeriod with per-call
// audit actor attribution (LG-73).
func (l *Ledger) SetWebhookKeyGracePeriodCtx(ctx context.Context, name string, d time.Duration) (time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.webhooks == nil {
		return 0, ErrWebhooksNotConfigured
	}
	previous, err := l.webhooks.SetKeyGracePeriod(name, d)
	if err != nil {
		return previous, err
	}
	if d == previous {
		return previous, nil
	}
	l.emitAuditCtx(ctx, AuditEvent{
		Op:            "webhook_key_grace_period_retuned",
		Actor:         "SetWebhookKeyGracePeriod",
		VersionBefore: l.version,
		VersionAfter:  l.version,
		Details: map[string]any{
			"endpoint":                 name,
			"previous_grace_period_ns": previous.Nanoseconds(),
			"current_grace_period_ns":  d.Nanoseconds(),
		},
	})
	return previous, nil
}

// WebhookKeyGracePeriod reports one registered endpoint's live
// signing-key grace period. It returns false when the ledger has no
// dispatcher attached or the name is unknown.
func (l *Ledger) WebhookKeyGracePeriod(name string) (time.Duration, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.webhooks == nil {
		return 0, false
	}
	return l.webhooks.KeyGracePeriod(name)
}
