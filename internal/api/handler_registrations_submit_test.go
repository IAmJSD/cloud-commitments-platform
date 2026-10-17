package api

// Tests for how submitRegistration maps store errors (issue #137): the 409
// "already registered" answer comes only from the typed duplicate sentinel,
// never from the word "duplicate" in an error message.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/email"
)

// countingRegistrationNotifier counts admin "registration received" emails.
type countingRegistrationNotifier struct {
	stubEmailNotifier
	received int
}

func (n *countingRegistrationNotifier) SendRegistrationReceivedNotification(_ context.Context, _ email.RegistrationNotificationData) error {
	n.received++
	return nil
}

const submitRegistrationBody = `{"provider":"aws","external_id":"123456789012",` +
	`"account_name":"Prod","contact_email":"owner@example.com"}`

func submitRegistrationRequest() *events.LambdaFunctionURLRequest {
	return &events.LambdaFunctionURLRequest{Body: submitRegistrationBody}
}

func TestSubmitRegistration_DuplicatePending_Returns409WithoutEmail(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	store.On("CreateAccountRegistration", ctx, mock.Anything).
		Return(fmt.Errorf("%w: %w", config.ErrDuplicatePendingRegistration,
			&pgconn.PgError{Code: "23505", ConstraintName: "idx_account_registrations_pending_unique"})).Once()
	notifier := &countingRegistrationNotifier{}
	h := &Handler{config: store, emailNotifier: notifier}

	_, err := h.submitRegistration(ctx, submitRegistrationRequest())

	ce, ok := IsClientError(err)
	require.True(t, ok, "duplicate must be a client error, got %v", err)
	assert.Equal(t, 409, ce.code)
	assert.Equal(t, "a pending registration already exists for this account", ce.message)
	assert.Zero(t, notifier.received, "a duplicate must not email admins")
	store.AssertNumberOfCalls(t, "CreateAccountRegistration", 1)
}

// Errors that are not the duplicate sentinel must surface as a generic 500,
// even when their text contains "duplicate", and must not leak the DB error.
func TestSubmitRegistration_NonDuplicateErrors_Return500(t *testing.T) {
	cases := map[string]error{
		"text mentions duplicate": errors.New(
			`duplicate: duplicate key value violates unique constraint "idx_account_registrations_pending_unique"`),
		"unique violation on another constraint": fmt.Errorf("failed to create account registration: %w",
			&pgconn.PgError{Code: "23505", ConstraintName: "account_registrations_pkey",
				Message: "duplicate key value violates unique constraint"}),
		"other database error": fmt.Errorf("failed to create account registration: %w",
			&pgconn.PgError{Code: "53300", Message: "too many connections"}),
	}
	for name, storeErr := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := new(MockConfigStore)
			store.On("CreateAccountRegistration", ctx, mock.Anything).Return(storeErr).Once()
			notifier := &countingRegistrationNotifier{}
			h := &Handler{config: store, emailNotifier: notifier}

			_, err := h.submitRegistration(ctx, submitRegistrationRequest())

			require.Error(t, err)
			_, isClient := IsClientError(err)
			assert.False(t, isClient, "must not be reported as a client (409) error")
			status, body := h.handleRequestError(err)
			assert.Equal(t, 500, status)
			assert.Equal(t, map[string]string{"error": "Internal server error"}, body)
			assert.Zero(t, notifier.received, "a duplicate must not email admins")
			store.AssertNumberOfCalls(t, "CreateAccountRegistration", 1)
		})
	}
}

// A reference-token collision is retryable: the handler regenerates the token
// and re-inserts instead of telling the submitter the account is registered.
func TestSubmitRegistration_TokenCollision_RetriesWithNewToken(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	var tokens []string
	collision := fmt.Errorf("%w: %w", config.ErrReferenceTokenCollision,
		&pgconn.PgError{Code: "23505", ConstraintName: "account_registrations_reference_token_key"})
	record := func(args mock.Arguments) {
		tokens = append(tokens, args.Get(1).(*config.AccountRegistration).ReferenceToken)
	}
	store.On("CreateAccountRegistration", ctx, mock.Anything).Run(record).Return(collision).Once()
	store.On("CreateAccountRegistration", ctx, mock.Anything).Run(record).Return(nil).Once()
	h := &Handler{config: store}

	resp, err := h.submitRegistration(ctx, submitRegistrationRequest())

	require.NoError(t, err)
	require.Len(t, tokens, 2)
	assert.NotEqual(t, tokens[0], tokens[1], "retry must use a fresh token")
	assert.Equal(t, map[string]string{"reference_token": tokens[1], "status": "pending"}, resp)
}

func TestSubmitRegistration_TokenCollision_GivesUpAs500(t *testing.T) {
	ctx := context.Background()
	store := new(MockConfigStore)
	collision := fmt.Errorf("%w: %w", config.ErrReferenceTokenCollision,
		&pgconn.PgError{Code: "23505", ConstraintName: "account_registrations_reference_token_key"})
	store.On("CreateAccountRegistration", ctx, mock.Anything).Return(collision)
	h := &Handler{config: store}

	_, err := h.submitRegistration(ctx, submitRegistrationRequest())

	require.ErrorIs(t, err, config.ErrReferenceTokenCollision)
	_, isClient := IsClientError(err)
	assert.False(t, isClient)
	store.AssertNumberOfCalls(t, "CreateAccountRegistration", maxReferenceTokenAttempts)
}
