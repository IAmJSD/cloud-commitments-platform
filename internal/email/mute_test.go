package email

import (
	"context"
	"errors"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/common"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Stub MuteChecker
// ---------------------------------------------------------------------------

type mockMuteChecker struct {
	mock.Mock
}

func setMuteTestSecret(t *testing.T) {
	t.Helper()
	t.Setenv("NOTIFICATION_MUTE_SECRET", "email-mute-test-secret")
}

func (m *mockMuteChecker) IsNotificationMuted(ctx context.Context, email, scope string) (bool, error) {
	args := m.Called(ctx, email, scope)
	return args.Bool(0), args.Error(1)
}

// ---------------------------------------------------------------------------
// SendPurchaseApprovalRequest + mute check
// ---------------------------------------------------------------------------

// newSenderWithMute builds a testable *Sender with a mock SES client and a
// mock MuteChecker, bypassing sandbox checks by having GetAccount return
// production mode.
func newSenderWithMute(ses *MockSESClient, mc *mockMuteChecker) *Sender {
	// GetAccount returning ProductionAccessEnabled=true means no sandbox path.
	ses.On("GetAccount", mock.Anything, mock.Anything).
		Return(&sesv2.GetAccountOutput{ProductionAccessEnabled: true}, nil).Maybe()
	return &Sender{
		sesClient:   ses,
		fromEmail:   "noreply@example.com",
		muteChecker: mc,
	}
}

func TestSendPurchaseApprovalRequest_MutedRecipient_NoSESCall(t *testing.T) {
	ctx := context.Background()
	ses := new(MockSESClient)
	mc := new(mockMuteChecker)

	mc.On("IsNotificationMuted", mock.Anything, "approver@example.com", string(common.ScopePurchaseApprovals)).
		Return(true, nil)
	t.Cleanup(func() {
		mc.AssertExpectations(t)
		ses.AssertNotCalled(t, "SendEmail", mock.Anything, mock.Anything)
	})

	s := newSenderWithMute(ses, mc)

	data := NotificationData{
		RecipientEmail: "approver@example.com",
		Recommendations: []RecommendationSummary{
			{Service: "ec2", Region: "us-east-1", Count: 1, MonthlySavings: 100},
		},
		DashboardURL:  "https://dashboard.example.com",
		ApprovalToken: "tok",
	}
	err := s.SendPurchaseApprovalRequest(ctx, data)
	require.NoError(t, err)
}

func TestSendPurchaseApprovalRequest_NotMuted_SendsEmail(t *testing.T) {
	ctx := context.Background()
	ses := new(MockSESClient)
	mc := new(mockMuteChecker)

	mc.On("IsNotificationMuted", mock.Anything, "approver@example.com", string(common.ScopePurchaseApprovals)).
		Return(false, nil)
	mc.On("IsNotificationMuted", mock.Anything, mock.Anything, mock.Anything).
		Return(false, nil).Maybe() // for CC filter if any
	ses.On("GetAccount", mock.Anything, mock.Anything).
		Return(&sesv2.GetAccountOutput{ProductionAccessEnabled: true}, nil)
	ses.On("SendEmail", mock.Anything, mock.Anything).
		Return(&sesv2.SendEmailOutput{}, nil)
	t.Cleanup(func() {
		mc.AssertExpectations(t)
		ses.AssertExpectations(t)
	})

	s := newSenderWithMute(ses, mc)
	data := NotificationData{
		RecipientEmail: "approver@example.com",
		Recommendations: []RecommendationSummary{
			{Service: "ec2", Region: "us-east-1", Count: 1, MonthlySavings: 100},
		},
		DashboardURL:  "https://dashboard.example.com",
		ApprovalToken: "tok",
	}
	err := s.SendPurchaseApprovalRequest(ctx, data)
	require.NoError(t, err)
	ses.AssertCalled(t, "SendEmail", mock.Anything, mock.Anything)
}

func TestSendPurchaseApprovalRequest_MuteCheckError_FailOpen(t *testing.T) {
	// When the mute store returns an error, the email is still sent (fail-open
	// so a DB hiccup doesn't permanently block approval notifications).
	ctx := context.Background()
	ses := new(MockSESClient)
	mc := new(mockMuteChecker)

	mc.On("IsNotificationMuted", mock.Anything, "approver@example.com", string(common.ScopePurchaseApprovals)).
		Return(false, errors.New("db error"))
	mc.On("IsNotificationMuted", mock.Anything, mock.Anything, mock.Anything).
		Return(false, nil).Maybe()
	ses.On("GetAccount", mock.Anything, mock.Anything).
		Return(&sesv2.GetAccountOutput{ProductionAccessEnabled: true}, nil)
	ses.On("SendEmail", mock.Anything, mock.Anything).
		Return(&sesv2.SendEmailOutput{}, nil)
	t.Cleanup(func() {
		ses.AssertCalled(t, "SendEmail", mock.Anything, mock.Anything)
	})

	s := newSenderWithMute(ses, mc)
	data := NotificationData{
		RecipientEmail: "approver@example.com",
		Recommendations: []RecommendationSummary{
			{Service: "ec2", Region: "us-east-1", Count: 1, MonthlySavings: 100},
		},
		DashboardURL:  "https://dashboard.example.com",
		ApprovalToken: "tok",
	}
	err := s.SendPurchaseApprovalRequest(ctx, data)
	require.NoError(t, err)
}

func TestSendRIExchangePendingApproval_MutedRecipient_NoSESCall(t *testing.T) {
	ctx := context.Background()
	ses := new(MockSESClient)
	mc := new(mockMuteChecker)
	mc.On("IsNotificationMuted", mock.Anything, "ri-approver@example.com", string(common.ScopeRIExchangeApprovals)).
		Return(true, nil).Once()
	t.Cleanup(func() {
		mc.AssertExpectations(t)
		ses.AssertNotCalled(t, "SendEmail", mock.Anything, mock.Anything)
	})

	s := newSenderWithMute(ses, mc)
	err := s.SendRIExchangePendingApproval(ctx, RIExchangeNotificationData{
		RecipientEmail: "ri-approver@example.com",
		DashboardURL:   "https://dash.example.com",
		Exchanges:      []RIExchangeItem{{RecordID: "rec-1", ApprovalToken: "tok"}},
	})
	require.NoError(t, err)
}

func TestSendRIExchangePendingApproval_EmitsScopedUnsubscribeHeaders(t *testing.T) {
	setMuteTestSecret(t)
	ctx := context.Background()
	ses := new(MockSESClient)
	mc := new(mockMuteChecker)
	mc.On("IsNotificationMuted", mock.Anything, "ri-approver@example.com", string(common.ScopeRIExchangeApprovals)).
		Return(false, nil).Once()

	var captured *sesv2.SendEmailInput
	ses.On("SendEmail", mock.Anything, mock.MatchedBy(func(in *sesv2.SendEmailInput) bool {
		captured = in
		return true
	})).Return(&sesv2.SendEmailOutput{}, nil).Once()
	t.Cleanup(func() {
		mc.AssertExpectations(t)
		ses.AssertExpectations(t)
	})

	s := newSenderWithMute(ses, mc).WithUnsubscribeBaseURL("https://dash.example.com")
	err := s.SendRIExchangePendingApproval(ctx, RIExchangeNotificationData{
		RecipientEmail: "ri-approver@example.com",
		DashboardURL:   "https://dash.example.com",
		Exchanges:      []RIExchangeItem{{RecordID: "rec-1", ApprovalToken: "tok"}},
	})
	require.NoError(t, err)
	require.NotNil(t, captured)
	require.NotNil(t, captured.Content.Simple)
	require.Len(t, captured.Content.Simple.Headers, 2)
	assert.Equal(t, "List-Unsubscribe", *captured.Content.Simple.Headers[0].Name)
	assert.Contains(t, *captured.Content.Simple.Headers[0].Value, "scope="+string(common.ScopeRIExchangeApprovals))
	assert.NotContains(t, *captured.Content.Simple.Headers[0].Value, "scope="+string(common.ScopePurchaseApprovals))
	assert.Equal(t, "List-Unsubscribe=One-Click", *captured.Content.Simple.Headers[1].Value)
}

// TestSendPurchaseApprovalRequest_WithCC_SuppressesListUnsubscribe verifies the
// List-Unsubscribe header (whose token is bound to the primary recipient) is NOT
// emitted when the message also goes to CC recipients. A shared-envelope CC
// recipient could otherwise one-click-mute the primary recipient.
func TestSendPurchaseApprovalRequest_WithCC_SuppressesListUnsubscribe(t *testing.T) {
	ctx := context.Background()
	ses := new(MockSESClient)
	mc := new(mockMuteChecker)

	mc.On("IsNotificationMuted", mock.Anything, mock.Anything, mock.Anything).
		Return(false, nil)
	ses.On("GetAccount", mock.Anything, mock.Anything).
		Return(&sesv2.GetAccountOutput{ProductionAccessEnabled: true}, nil)

	var captured *sesv2.SendEmailInput
	ses.On("SendEmail", mock.Anything, mock.MatchedBy(func(in *sesv2.SendEmailInput) bool {
		captured = in
		return true
	})).Return(&sesv2.SendEmailOutput{}, nil)
	t.Cleanup(func() { mc.AssertExpectations(t) })

	s := newSenderWithMute(ses, mc).WithUnsubscribeBaseURL("https://dash.example.com")
	data := NotificationData{
		RecipientEmail: "approver@example.com",
		CCEmails:       []string{"observer@example.com"},
		Recommendations: []RecommendationSummary{
			{Service: "ec2", Region: "us-east-1", Count: 1, MonthlySavings: 100},
		},
		DashboardURL:  "https://dashboard.example.com",
		ApprovalToken: "tok",
	}
	require.NoError(t, s.SendPurchaseApprovalRequest(ctx, data))
	require.NotNil(t, captured)
	require.NotNil(t, captured.Content.Simple)
	for _, h := range captured.Content.Simple.Headers {
		if h.Name != nil {
			assert.NotEqual(t, "List-Unsubscribe", *h.Name,
				"List-Unsubscribe must be suppressed when CC recipients are present")
		}
	}
}

// ---------------------------------------------------------------------------
// List-Unsubscribe header injection
// ---------------------------------------------------------------------------

func TestBuildUnsubscribeURL_EmptyBaseURL_ReturnsEmpty(t *testing.T) {
	s := &Sender{}
	u := s.buildUnsubscribeURL("user@example.com", "purchase_approvals")
	assert.Empty(t, u)
}

func TestBuildUnsubscribeURL_WithBaseURL_ContainsParams(t *testing.T) {
	setMuteTestSecret(t)
	s := &Sender{unsubscribeBaseURL: "https://dash.example.com"}
	u := s.buildUnsubscribeURL("user@example.com", "purchase_approvals")
	assert.Contains(t, u, "email=user%40example.com")
	assert.Contains(t, u, "scope=purchase_approvals")
	assert.Contains(t, u, "token=")
}

func TestListUnsubscribeHeaders_EmptyBase_ReturnsEmpty(t *testing.T) {
	s := &Sender{}
	hdr, post := s.listUnsubscribeHeaders("u@e.com", "purchase_approvals")
	assert.Empty(t, hdr)
	assert.Empty(t, post)
}

func TestListUnsubscribeHeaders_WithBase(t *testing.T) {
	setMuteTestSecret(t)
	s := &Sender{unsubscribeBaseURL: "https://dash.example.com"}
	hdr, post := s.listUnsubscribeHeaders("u@e.com", "purchase_approvals")
	assert.Contains(t, hdr, "<https://dash.example.com/api/notifications/unsubscribe?")
	assert.Equal(t, "List-Unsubscribe=One-Click", post)
}

func TestAddListUnsubscribeHeaders_EmptyValue_ReturnsNil(t *testing.T) {
	hdrs := addListUnsubscribeHeaders("", "")
	assert.Nil(t, hdrs)
}

func TestAddListUnsubscribeHeaders_WithValues(t *testing.T) {
	hdrs := addListUnsubscribeHeaders("<https://example.com/unsub>", "List-Unsubscribe=One-Click")
	require.Len(t, hdrs, 2)
	assert.Equal(t, "List-Unsubscribe", *hdrs[0].Name)
	assert.Equal(t, "List-Unsubscribe-Post", *hdrs[1].Name)
	assert.Equal(t, "List-Unsubscribe=One-Click", *hdrs[1].Value)
}

// ---------------------------------------------------------------------------
// DeriveMuteToken / VerifyMuteToken
// ---------------------------------------------------------------------------

func TestDeriveMuteToken_Stable(t *testing.T) {
	key := []byte("test-secret")
	t1 := common.DeriveMuteToken(key, "user@example.com", "purchase_approvals")
	t2 := common.DeriveMuteToken(key, "user@example.com", "purchase_approvals")
	assert.Equal(t, t1, t2)
}

func TestDeriveMuteToken_CaseInsensitive(t *testing.T) {
	key := []byte("test-secret")
	lower := common.DeriveMuteToken(key, "user@example.com", "purchase_approvals")
	upper := common.DeriveMuteToken(key, "USER@EXAMPLE.COM", "purchase_approvals")
	assert.Equal(t, lower, upper, "token must be case-insensitive on email")
}

func TestDeriveMuteToken_DiffersByScope(t *testing.T) {
	key := []byte("test-secret")
	t1 := common.DeriveMuteToken(key, "user@example.com", "purchase_approvals")
	t2 := common.DeriveMuteToken(key, "user@example.com", "ri_exchange_approvals")
	assert.NotEqual(t, t1, t2)
}

func TestVerifyMuteToken_Valid(t *testing.T) {
	key := []byte("test-secret")
	tok := common.DeriveMuteToken(key, "user@example.com", "purchase_approvals")
	assert.True(t, common.VerifyMuteToken(key, "user@example.com", "purchase_approvals", tok))
}

func TestVerifyMuteToken_Forged(t *testing.T) {
	key := []byte("test-secret")
	assert.False(t, common.VerifyMuteToken(key, "user@example.com", "purchase_approvals", "forgedtoken"))
}

// TestIsRecipientMuted_StoreErrorDirectionPerScope pins the per-scope failure
// direction of a mute-store error (issue #111). The approval scopes fail open
// (send), because dropping an approval email strands a purchase or exchange.
// Every other scope is informational and fails closed (don't send), so a DB
// blip cannot re-mail someone who used the one-click unsubscribe link.
func TestIsRecipientMuted_StoreErrorDirectionPerScope(t *testing.T) {
	const addr = "ops@example.com"
	cases := []struct {
		scope     string
		wantMuted bool
	}{
		{scope: string(common.ScopePurchaseApprovals), wantMuted: false},
		{scope: string(common.ScopeRIExchangeApprovals), wantMuted: false},
		// No informational scope is defined yet; any scope outside the
		// approval set is treated as informational.
		{scope: "purchase_executed", wantMuted: true},
		{scope: "new_recommendations", wantMuted: true},
		{scope: "", wantMuted: true},
	}
	for _, tc := range cases {
		t.Run("scope="+tc.scope, func(t *testing.T) {
			mc := new(mockMuteChecker)
			mc.On("IsNotificationMuted", mock.Anything, addr, tc.scope).
				Return(false, errors.New("db error"))

			assert.Equal(t, tc.wantMuted, isRecipientMuted(context.Background(), mc, addr, tc.scope))

			// The CC filter and the primary-recipient check follow the same direction.
			kept := filterMutedRecipients(context.Background(), mc, []string{addr}, tc.scope)
			if tc.wantMuted {
				assert.Empty(t, kept, "informational scope must drop a CC whose mute state is unknown")
			} else {
				assert.Equal(t, []string{addr}, kept, "approval scope must keep a CC whose mute state is unknown")
			}
			_, _, _, muted := prepareMuteAwareDelivery(context.Background(), mc, "", addr, nil, tc.scope)
			assert.Equal(t, tc.wantMuted, muted)
			mc.AssertExpectations(t)
		})
	}
}

// TestIsRecipientMuted_NoErrorFollowsStore checks that the per-scope error
// policy does not change the answer when the store responds normally.
func TestIsRecipientMuted_NoErrorFollowsStore(t *testing.T) {
	for _, scope := range []string{string(common.ScopePurchaseApprovals), "new_recommendations"} {
		for _, stored := range []bool{true, false} {
			mc := new(mockMuteChecker)
			mc.On("IsNotificationMuted", mock.Anything, "a@example.com", scope).Return(stored, nil)
			assert.Equal(t, stored, isRecipientMuted(context.Background(), mc, "a@example.com", scope), "scope=%s stored=%v", scope, stored)
		}
	}
	// A nil checker means muting is not configured, so nothing is muted.
	assert.False(t, isRecipientMuted(context.Background(), nil, "a@example.com", "new_recommendations"))
}
