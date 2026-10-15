package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/credentials"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression tests for #722: POST /api/accounts/{id}/test must run AWS
// accounts through the same credential resolver collection and purchases use
// and finish with an sts:GetCallerIdentity probe, so it cannot report OK for a
// config the resolver rejects.

const (
	probeAccountID   = "11111111-1111-1111-1111-111111111111"
	probeHostKeyID   = "AKIAPROBEHOST000000"
	probeMemberKeyID = "AKIAPROBEMEMBER0000"
	probeAssumedKey  = "ASIAPROBEASSUMED000"
	probeHostAcct    = "111111111111"
	probeTenantAcct  = "222222222222"
	probeTenantRole  = "arn:aws:iam::222222222222:role/tenant"
)

// probeSTS fakes the STS endpoint. GetCallerIdentity answers as whichever
// identity signed the request; AssumeRole hands out probeAssumedKey unless
// denyAssumeRole is set.
type probeSTS struct {
	denyAssumeRole bool
	assumeRoles    atomic.Int32
	identityCalls  atomic.Int32
}

func probeXML(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{"Content-Type": []string{"text/xml"}}}
}

func (p *probeSTS) Do(r *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(r.Body)
	body := string(b)
	switch {
	case strings.Contains(body, "Action=AssumeRole"):
		p.assumeRoles.Add(1)
		if p.denyAssumeRole {
			return probeXML(403, `<ErrorResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><Error><Type>Sender</Type><Code>AccessDenied</Code><Message>not authorized to perform: sts:AssumeRole</Message></Error><RequestId>r</RequestId></ErrorResponse>`), nil
		}
		return probeXML(200, `<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleResult><Credentials><AccessKeyId>`+probeAssumedKey+
			`</AccessKeyId><SecretAccessKey>s</SecretAccessKey><SessionToken>t</SessionToken><Expiration>2099-01-01T00:00:00Z</Expiration></Credentials>`+
			`<AssumedRoleUser><AssumedRoleId>A:s</AssumedRoleId><Arn>arn:aws:sts::`+probeTenantAcct+`:assumed-role/tenant/s</Arn></AssumedRoleUser></AssumeRoleResult>`+
			`<ResponseMetadata><RequestId>r</RequestId></ResponseMetadata></AssumeRoleResponse>`), nil
	case strings.Contains(body, "Action=GetCallerIdentity"):
		p.identityCalls.Add(1)
		auth := r.Header.Get("Authorization")
		acct, arn := probeHostAcct, "arn:aws:iam::"+probeHostAcct+":role/host"
		switch {
		case strings.Contains(auth, probeMemberKeyID):
			acct, arn = probeTenantAcct, "arn:aws:iam::"+probeTenantAcct+":user/member"
		case strings.Contains(auth, probeAssumedKey):
			acct, arn = probeTenantAcct, "arn:aws:sts::"+probeTenantAcct+":assumed-role/tenant/s"
		}
		return probeXML(200, `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Arn>`+arn+
			`</Arn><UserId>U</UserId><Account>`+acct+`</Account></GetCallerIdentityResult><ResponseMetadata><RequestId>r</RequestId></ResponseMetadata></GetCallerIdentityResponse>`), nil
	}
	return probeXML(400, `<ErrorResponse><Error><Code>Unexpected</Code><Message>unexpected STS call</Message></Error></ErrorResponse>`), nil
}

// probeCredStore reports a stored credential for every lookup and returns
// payload from LoadRaw.
type probeCredStore struct {
	MockCredentialStore
	payload string
}

func (s *probeCredStore) HasCredential(context.Context, string, string) (bool, error) {
	return true, nil
}

func (s *probeCredStore) LoadRaw(context.Context, string, string) ([]byte, error) {
	if s.payload == "" {
		return nil, nil
	}
	return []byte(s.payload), nil
}

const validAccessKeyPayload = `{"access_key_id":"` + probeMemberKeyID + `","secret_access_key":"member-secret"}`

func runAccountTest(t *testing.T, acct *config.CloudAccount, payload string, stsFake *probeSTS) AccountTestResult {
	t.Helper()
	ctx := context.Background()
	mockAuth := new(MockAuthService)
	mockAuth.On("ValidateSession", ctx, "admin-token").Return(&Session{UserID: "uid"}, nil)
	mockAuth.grantAdmin()

	store := new(MockConfigStore)
	acct.ID = probeAccountID
	acct.Provider = "aws"
	store.GetCloudAccountFn = func(context.Context, string) (*config.CloudAccount, error) { return acct, nil }

	h := &Handler{auth: mockAuth, config: store, credStore: &probeCredStore{payload: payload}}
	h.awsCfgOnce.Do(func() {
		h.awsCfg = aws.Config{
			Region:      "us-east-1",
			Credentials: awscreds.NewStaticCredentialsProvider(probeHostKeyID, "host-secret", ""),
			HTTPClient:  stsFake,
		}
	})
	req := &events.LambdaFunctionURLRequest{Headers: map[string]string{"Authorization": "Bearer admin-token"}}
	res, err := h.testAccountCredentials(ctx, req, probeAccountID)
	require.NoError(t, err)
	out, ok := res.(AccountTestResult)
	require.True(t, ok, "unexpected result type %T", res)
	return out
}

// TestTestAccountCredentials_AWSResolverRejects covers configs the old
// shape-only check reported OK for but the resolver refuses.
func TestTestAccountCredentials_AWSResolverRejects(t *testing.T) {
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "")
	tests := []struct {
		name    string
		acct    *config.CloudAccount
		payload string
		deny    bool
		wantMsg string
	}{
		{
			name:    "empty auth mode with stored access keys",
			acct:    &config.CloudAccount{ExternalID: probeTenantAcct, AWSAuthMode: ""},
			payload: validAccessKeyPayload,
			wantMsg: "unsupported aws_auth_mode",
		},
		{
			name:    "bastion mode without bastion id",
			acct:    &config.CloudAccount{ExternalID: probeTenantAcct, AWSAuthMode: "bastion", AWSRoleARN: probeTenantRole},
			wantMsg: "aws_bastion_id is required",
		},
		{
			name:    "bastion mode without role ARN",
			acct:    &config.CloudAccount{ExternalID: probeTenantAcct, AWSAuthMode: "bastion", AWSBastionID: "b"},
			wantMsg: "aws_role_arn is empty",
		},
		{
			name:    "web identity without token file",
			acct:    &config.CloudAccount{ExternalID: probeTenantAcct, AWSAuthMode: "workload_identity_federation", AWSRoleARN: probeTenantRole},
			wantMsg: "aws_web_identity_token_file required",
		},
		{
			name:    "role_arn without role ARN on a non-host account",
			acct:    &config.CloudAccount{ExternalID: probeTenantAcct, AWSAuthMode: "role_arn"},
			wantMsg: credentials.ErrNotHostAccount.Error(),
		},
		{
			name:    "access_keys with incomplete stored payload",
			acct:    &config.CloudAccount{ExternalID: probeTenantAcct, AWSAuthMode: "access_keys"},
			payload: `{"access_key_id":"` + probeMemberKeyID + `"}`,
			wantMsg: "secret_access_key is empty",
		},
		{
			name:    "role_arn whose AssumeRole is denied",
			acct:    &config.CloudAccount{ExternalID: probeTenantAcct, AWSAuthMode: "role_arn", AWSRoleARN: probeTenantRole},
			deny:    true,
			wantMsg: "credential retrieval failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := runAccountTest(t, tt.acct, tt.payload, &probeSTS{denyAssumeRole: tt.deny})
			assert.False(t, res.OK, "test endpoint reported OK for a config the resolver rejects: %q", res.Message)
			assert.Contains(t, res.Message, tt.wantMsg)
		})
	}
}

func TestTestAccountCredentials_AWSValidConfigsProbeCallerIdentity(t *testing.T) {
	tests := []struct {
		name       string
		acct       *config.CloudAccount
		payload    string
		wantArn    string
		wantAssume int32
	}{
		{
			name:    "access_keys",
			acct:    &config.CloudAccount{ExternalID: probeTenantAcct, AWSAuthMode: "access_keys"},
			payload: validAccessKeyPayload,
			wantArn: "arn:aws:iam::" + probeTenantAcct + ":user/member",
		},
		{
			name:       "role_arn with role ARN",
			acct:       &config.CloudAccount{ExternalID: probeTenantAcct, AWSAuthMode: "role_arn", AWSRoleARN: probeTenantRole},
			wantArn:    "arn:aws:sts::" + probeTenantAcct + ":assumed-role/tenant/s",
			wantAssume: 1,
		},
		{
			name:    "role_arn without role ARN on the host account",
			acct:    &config.CloudAccount{ExternalID: probeHostAcct, AWSAuthMode: "role_arn"},
			wantArn: "arn:aws:iam::" + probeHostAcct + ":role/host",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &probeSTS{}
			res := runAccountTest(t, tt.acct, tt.payload, fake)
			assert.True(t, res.OK, "unexpected failure: %q", res.Message)
			assert.Contains(t, res.Message, tt.wantArn)
			assert.Equal(t, tt.wantAssume, fake.assumeRoles.Load())
			assert.GreaterOrEqual(t, fake.identityCalls.Load(), int32(1), "sts:GetCallerIdentity probe must run")
		})
	}
}
