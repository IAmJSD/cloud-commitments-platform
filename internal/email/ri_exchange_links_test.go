package email

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Issue #579: the RI-exchange emails must link to the SPA deep link
// (/inventory/ri-exchange/{approve,reject}/:id?token=), which confirms and
// then POSTs. The API paths only route POST, so a click (a GET) returned 401.
// The history links must open the RI Exchange sub-tab; /#ri-exchange landed
// on Home because the SPA routes by path, not by hash.

const riExchangeLinksDashboard = "https://dashboard.example.com"

func riExchangeLinksData() RIExchangeNotificationData {
	return RIExchangeNotificationData{
		DashboardURL: riExchangeLinksDashboard,
		TotalPayment: "125.50",
		Exchanges: []RIExchangeItem{{
			RecordID: "rec-579", ApprovalToken: "tok/+&=",
			SourceRIID: "ri-0123456789abcdef0", SourceInstanceType: "m5.large",
			TargetInstanceType: "m6i.large", TargetCount: 1,
			PaymentDue: "125.50", UtilizationPct: 42.5,
		}},
	}
}

const (
	riExchangeApproveLink = riExchangeLinksDashboard + "/inventory/ri-exchange/approve/rec-579?token=tok%2F%2B%26%3D"
	riExchangeRejectLink  = riExchangeLinksDashboard + "/inventory/ri-exchange/reject/rec-579?token=tok%2F%2B%26%3D"
	riExchangeHistoryLink = riExchangeLinksDashboard + "/inventory/ri-exchange"
)

// assertNoBrokenRIExchangeLinks rejects the two link shapes #579 replaced.
func assertNoBrokenRIExchangeLinks(t *testing.T, body string) {
	t.Helper()
	assert.NotContains(t, body, "/api/ri-exchange/approve/", "approve link must not hit the POST-only API route")
	assert.NotContains(t, body, "/api/ri-exchange/reject/", "reject link must not hit the POST-only API route")
	assert.NotContains(t, body, "/#ri-exchange", "history link must use the /inventory/ri-exchange path")
}

func TestRIExchangePendingApprovalEmail_LinksToSPADeepLink(t *testing.T) {
	body, err := RenderRIExchangePendingApprovalEmail(riExchangeLinksData())
	require.NoError(t, err)

	assert.Contains(t, body, "Approve: "+riExchangeApproveLink+"\n")
	assert.Contains(t, body, "Reject:  "+riExchangeRejectLink+"\n")
	assert.Contains(t, body, "\n"+riExchangeHistoryLink+"\n")
	assertNoBrokenRIExchangeLinks(t, body)
}

func TestRIExchangePendingApprovalEmailHTML_LinksToSPADeepLink(t *testing.T) {
	html, err := RenderRIExchangePendingApprovalEmailHTML(riExchangeLinksData())
	require.NoError(t, err)

	assert.Contains(t, html, `href="`+riExchangeApproveLink+`"`)
	assert.Contains(t, html, `href="`+riExchangeRejectLink+`"`)
	assert.Contains(t, html, `href="`+riExchangeHistoryLink+`"`)
	assertNoBrokenRIExchangeLinks(t, html)
}

func TestRIExchangeCompletedEmail_HistoryLinkOpensRIExchangeTab(t *testing.T) {
	body, err := RenderRIExchangeCompletedEmail(outcomeUnknownExchangeData())
	require.NoError(t, err)

	assert.Contains(t, body, "View exchange history:\n"+riExchangeHistoryLink+"\n")
	assert.NotContains(t, body, "/#ri-exchange")
}
