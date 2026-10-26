package mocks

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/stretchr/testify/mock"
)

// MockSESClient is a mock implementation of SES client.
type MockSESClient struct { //nolint:revive // exported: doc comment style intentional
	mock.Mock
	// reporter fails the registered test on a mistyped .Return value. See
	// report.go.
	reporter testReporter
}

// SendEmail mocks the SendEmail operation.
func (m *MockSESClient) SendEmail(ctx context.Context, input *sesv2.SendEmailInput, opts ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error) {
	args := m.Called(ctx, input)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	v, ok := args.Get(0).(*sesv2.SendEmailOutput)
	if !ok {
		m.reporter.wrongReturnType("SendEmail", "*sesv2.SendEmailOutput", args.Get(0))
	}
	return v, args.Error(1)
}

// SESAPI defines the interface for SES operations used by our code.
type SESAPI interface {
	SendEmail(ctx context.Context, input *sesv2.SendEmailInput, opts ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error)
}

// Ensure MockSESClient implements SESAPI.
var _ SESAPI = (*MockSESClient)(nil)
