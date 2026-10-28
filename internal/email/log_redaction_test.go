package email

import (
	"bytes"
	"context"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/logging"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// captureDebugLogs routes the default logger to a buffer at Debug level and
// restores the previous output and level when the test ends.
func captureDebugLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prevOut, prevLevel := logging.SetOutput(&logs), logging.GetLevel()
	t.Cleanup(func() {
		logging.SetOutput(prevOut)
		logging.SetLevelValue(prevLevel)
	})
	logging.SetLevelValue(logging.LevelDebug)
	return &logs
}

// TestCreateVerificationRequest_RedactsAddressInLog covers issue #111: the SES
// sandbox verification path logged the full recipient address at Info.
func TestCreateVerificationRequest_RedactsAddressInLog(t *testing.T) {
	logs := captureDebugLogs(t)
	ses := new(MockSESClient)
	ses.On("CreateEmailIdentity", mock.Anything, mock.AnythingOfType("*sesv2.CreateEmailIdentityInput")).
		Return(&sesv2.CreateEmailIdentityOutput{}, nil)
	s := &Sender{sesClient: ses, fromEmail: "noreply@example.com"}

	require.NoError(t, s.createVerificationRequest(context.Background(), "customer.person@example.com"))

	out := logs.String()
	assert.Contains(t, out, "Created email verification request for cu***@example.com")
	assert.NotContains(t, out, "customer.person@example.com")
}

// TestSMTPSender_SendLogsRedactRecipient covers issue #111 (A06-012): the SMTP
// single-part and multipart send paths logged the raw To address at Debug,
// with and without CC recipients.
func TestSMTPSender_SendLogsRedactRecipient(t *testing.T) {
	const to = "customer.person@example.com"
	cases := []struct {
		name string
		send func(s *SMTPSender, cc []string) error
	}{
		{name: "text", send: func(s *SMTPSender, cc []string) error {
			return s.SendToEmailWithCC(context.Background(), to, cc, "Subject", "body")
		}},
		{name: "multipart", send: func(s *SMTPSender, cc []string) error {
			return s.SendToEmailWithCCMultipart(context.Background(), to, cc, "Subject", "body", "<p>body</p>")
		}},
	}
	for _, tc := range cases {
		for _, cc := range [][]string{nil, {"other.person@example.com"}} {
			t.Run(tc.name, func(t *testing.T) {
				logs := captureDebugLogs(t)
				server := newMockSMTPServer(t, false)
				server.start(t)
				s := &SMTPSender{host: "127.0.0.1", port: server.port, fromEmail: "notifications@example.com"}

				err := tc.send(s, cc)
				server.stop()
				require.NoError(t, err)

				out := logs.String()
				assert.Contains(t, out, "via SMTP to cu***@example.com", "send must be logged with the redacted address")
				assert.NotContains(t, out, to)
				assert.NotContains(t, out, "other.person@example.com")
			})
		}
	}
}
