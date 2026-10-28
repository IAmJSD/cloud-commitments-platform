package email

import (
	"context"
	"net/url"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
)

// This file holds the transport-agnostic mute + List-Unsubscribe logic shared
// by the SES (*Sender) and SMTP (*SMTPSender) paths. Both transports must apply
// the same per-recipient mute suppression, CC filtering, and RFC 8058
// List-Unsubscribe headers; keeping the logic here (instead of duplicating it
// per transport) prevents the two paths from diverging.

// isRecipientMuted returns true when (email, scope) is muted. A nil checker
// means muting is not configured, so nothing is muted.
//
// When the mute store errors, the answer depends on the scope (see
// muteCheckFailsOpen): approval scopes fail open (send), so a transient DB
// outage does not strand a purchase or exchange waiting on its approval email;
// every other scope is informational and fails closed (don't send), so the
// outage does not re-mail someone who used the one-click unsubscribe link.
func isRecipientMuted(ctx context.Context, mc MuteChecker, email, scope string) bool {
	if mc == nil {
		return false
	}
	muted, err := mc.IsNotificationMuted(ctx, email, scope)
	if err != nil {
		failOpen := muteCheckFailsOpen(scope)
		logging.Warnf("email: mute check failed for scope=%s (sending anyway: %t): %v", scope, failOpen, err)
		return !failOpen
	}
	return muted
}

// muteCheckFailsOpen reports whether a mute-store error should let an email
// through for scope. Only the approval scopes do: their emails carry the links
// a pending purchase or RI exchange is waiting on. Any other scope, including
// one added later, is treated as informational and fails closed.
func muteCheckFailsOpen(scope string) bool {
	switch common.MuteNotifScope(scope) {
	case common.ScopePurchaseApprovals, common.ScopeRIExchangeApprovals:
		return true
	default:
		return false
	}
}

// filterMutedRecipients returns a copy of addrs with any address muted for scope
// removed. The original slice is not modified. A mute-store error follows the
// per-scope direction of isRecipientMuted.
func filterMutedRecipients(ctx context.Context, mc MuteChecker, addrs []string, scope string) []string {
	if mc == nil || len(addrs) == 0 {
		return addrs
	}
	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		if !isRecipientMuted(ctx, mc, addr, scope) {
			out = append(out, addr)
		}
	}
	return out
}

// prepareMuteAwareDelivery applies the shared approval-email mute policy and
// returns the filtered CC list plus RFC 8058 header values. Headers are omitted
// for shared envelopes because their token is bound to the primary recipient.
func prepareMuteAwareDelivery(ctx context.Context, mc MuteChecker, baseURL, recipient string, cc []string, scope string) (filteredCC []string, headerValue, postValue string, muted bool) {
	if isRecipientMuted(ctx, mc, recipient, scope) {
		return nil, "", "", true
	}
	filteredCC = filterMutedRecipients(ctx, mc, cc, scope)
	if len(filteredCC) == 0 {
		headerValue, postValue = unsubscribeHeaderValuesFor(baseURL, recipient, scope)
	}
	return filteredCC, headerValue, postValue, false
}

// unsubscribeURLFor constructs the one-click unsubscribe URL for the given
// (email, scope) pair. Returns "" when baseURL is empty or when no signing key
// is available (e.g. NOTIFICATION_MUTE_SECRET is unset), so a
// tokenless, non-functional unsubscribe link is never emitted.
func unsubscribeURLFor(baseURL, email, scope string) string {
	if baseURL == "" {
		return ""
	}
	token := common.DeriveMuteToken(muteKey(), email, scope)
	if token == "" {
		return ""
	}
	q := url.Values{
		"token": {token},
		"email": {email},
		"scope": {scope},
	}
	return baseURL + "/api/notifications/unsubscribe?" + q.Encode()
}

// unsubscribeHeaderValuesFor returns the List-Unsubscribe and
// List-Unsubscribe-Post header values (RFC 8058) for the given (email, scope)
// pair. Returns ("", "") when baseURL is empty.
func unsubscribeHeaderValuesFor(baseURL, email, scope string) (headerValue, postValue string) {
	unsubURL := unsubscribeURLFor(baseURL, email, scope)
	if unsubURL == "" {
		return "", ""
	}
	return "<" + unsubURL + ">", "List-Unsubscribe=One-Click"
}
