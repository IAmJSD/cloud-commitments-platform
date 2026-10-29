package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression tests for #722: POST /api/accounts/{id}/test must run AWS
// accounts through the same credential resolver collection and purchases use
// and finish with an sts:GetCallerIdentity probe whose account matches the
// row's external_id, so it cannot report OK for a config real use rejects.
// The response must carry only fixed, classified messages: no host identity,
// no foreign account IDs, no raw driver / SDK / STS error text.

const (
	probeAccountID   = "11111111-1111-1111-1111-111111111111"
	probeHostKeyID   = "AKIAPROBEHOST000000"
	probeMemberKeyID = "AKIAPROBEMEMBER0000"
	probeAssumedKey  = "ASIAPROBEASSUMED000"
	probeWebIdentKey = "ASIAPROBEWEBIDENT00"
	probeHostAcct    = "111111111111"
	probeTenantAcct  = "222222222222"
	probeOtherAcct   = "333333333333"
	probeTenantRole  = "arn:aws:iam::222222222222:role/tenant"
	probeHostRoleARN = "arn:aws:sts::111111111111:assumed-role/host/session"
	probeRequestID   = "req-leak-0000"
	probeDriverText  = "pq: driver-secret-text connection refused"
	probeProfileText = "failed to load profile secret-profile"
)

// leakMarkers must never appear in an account test response.
var leakMarkers = []string{probeHostAcct, probeHostRoleARN, "assumed-role/host", probeRequestID, probeDriverText, "driver-secret-text", "secret-profile", "not authorized to perform"}

// probeSTS fakes the STS endpoint. GetCallerIdentity answers as whichever
// identity signed the request; AssumeRole / AssumeRoleWithWebIdentity hand out
// session keys unless told to fail.
type probeSTS struct {
	denyAssumeRole   bool
	identityErrCode  string
	block            bool
	transportErr     bool
	assumeRoles      atomic.Int32
	webIdentityCalls atomic.Int32
	identityCalls    atomic.Int32
	mu               sync.Mutex
	assumeSigners    []string // access key that signed each AssumeRole
}

// assumeRoleSigners returns the access key IDs that signed each AssumeRole.
func (p *probeSTS) assumeRoleSigners() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.assumeSigners...)
}

// signerKey returns the access key ID in a SigV4 Authorization header.
func signerKey(auth string) string {
	_, rest, ok := strings.Cut(auth, "Credential=")
	if !ok {
		return ""
	}
	key, _, _ := strings.Cut(rest, "/")
	return key
}

func probeXML(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{"Content-Type": []string{"text/xml"}}}
}

func probeError(status int, code, msg string) *http.Response {
	return probeXML(status, `<ErrorResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><Error><Type>Sender</Type><Code>`+code+`</Code><Message>`+msg+`</Message></Error><RequestId>`+probeRequestID+`</RequestId></ErrorResponse>`)
}

func probeCredsXML(op, key, arn string) string {
	return `<` + op + `Response xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><` + op + `Result><Credentials><AccessKeyId>` + key +
		`</AccessKeyId><SecretAccessKey>s</SecretAccessKey><SessionToken>t</SessionToken><Expiration>2099-01-01T00:00:00Z</Expiration></Credentials>` +
		`<AssumedRoleUser><AssumedRoleId>A:s</AssumedRoleId><Arn>` + arn + `</Arn></AssumedRoleUser></` + op + `Result>` +
		`<ResponseMetadata><RequestId>r</RequestId></ResponseMetadata></` + op + `Response>`
}

func (p *probeSTS) Do(r *http.Request) (*http.Response, error) {
	if p.block {
		<-r.Context().Done()
		return nil, r.Context().Err()
	}
	if p.transportErr {
		return nil, errors.New("dial tcp: lookup sts.amazonaws.com: no such host")
	}
	b, _ := io.ReadAll(r.Body)
	body := string(b)
	switch {
	case strings.Contains(body, "Action=AssumeRoleWithWebIdentity"):
		p.webIdentityCalls.Add(1)
		return probeXML(200, probeCredsXML("AssumeRoleWithWebIdentity", probeWebIdentKey, "arn:aws:sts::"+probeTenantAcct+":assumed-role/tenant/w")), nil
	case strings.Contains(body, "Action=AssumeRole"):
		p.assumeRoles.Add(1)
		p.mu.Lock()
		p.assumeSigners = append(p.assumeSigners, signerKey(r.Header.Get("Authorization")))
		p.mu.Unlock()
		if p.denyAssumeRole {
			return probeError(403, "AccessDenied", "User: "+probeHostRoleARN+" is not authorized to perform: sts:AssumeRole on resource: "+probeTenantRole), nil
		}
		return probeXML(200, probeCredsXML("AssumeRole", probeAssumedKey, "arn:aws:sts::"+probeTenantAcct+":assumed-role/tenant/s")), nil
	case strings.Contains(body, "Action=GetCallerIdentity"):
		p.identityCalls.Add(1)
		auth := r.Header.Get("Authorization")
		acct, arn := probeHostAcct, probeHostRoleARN
		switch {
		case strings.Contains(auth, probeMemberKeyID):
			acct, arn = probeTenantAcct, "arn:aws:iam::"+probeTenantAcct+":user/member"
		case strings.Contains(auth, probeAssumedKey), strings.Contains(auth, probeWebIdentKey):
			acct, arn = probeTenantAcct, "arn:aws:sts::"+probeTenantAcct+":assumed-role/tenant/s"
		}
		if p.identityErrCode != "" && acct != probeHostAcct {
			return probeError(403, p.identityErrCode, "The security token included in the request is invalid for "+probeHostRoleARN), nil
		}
		return probeXML(200, `<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Arn>`+arn+
			`</Arn><UserId>U</UserId><Account>`+acct+`</Account></GetCallerIdentityResult><ResponseMetadata><RequestId>r</RequestId></ResponseMetadata></GetCallerIdentityResponse>`), nil
	}
	return probeError(400, "Unexpected", "unexpected STS call"), nil
}

// probeCredStore reports a stored credential for every lookup and returns
// payload (or loadErr) from LoadRaw.
type probeCredStore struct {
	MockCredentialStore
	loadErr error
	payload string
}

func (s *probeCredStore) HasCredential(context.Context, string, string) (bool, error) {
	return true, nil
}

func (s *probeCredStore) LoadRaw(context.Context, string, string) ([]byte, error) {
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	if s.payload == "" {
		return nil, nil
	}
	return []byte(s.payload), nil
}

const validAccessKeyPayload = `{"access_key_id":"` + probeMemberKeyID + `","secret_access_key":"member-secret"}`

type probeCase struct {
	acct      *config.CloudAccount
	stsFake   *probeSTS
	loadErr   error
	hostCfgEr error
	payload   string
}

func runAccountTest(t *testing.T, pc probeCase) AccountTestResult {
	t.Helper()
	ctx := context.Background()
	mockAuth := new(MockAuthService)
	t.Cleanup(func() { mockAuth.AssertExpectations(t) })
	mockAuth.On("ValidateSession", ctx, "admin-token").Return(&Session{UserID: "uid"}, nil)
	mockAuth.grantAdmin()

	store := new(MockConfigStore)
	t.Cleanup(func() { store.AssertExpectations(t) })
	acct := pc.acct
	acct.ID = probeAccountID
	acct.Provider = "aws"
	// Any other ID is a bastion: an enabled access_keys account whose keys are
	// the same stored payload (issue #263: bastion accounts resolve through
	// their bastion's own credentials).
	store.GetCloudAccountFn = func(_ context.Context, id string) (*config.CloudAccount, error) {
		if id == probeAccountID {
			return acct, nil
		}
		return &config.CloudAccount{ID: id, Provider: "aws", Enabled: true, AWSAuthMode: "access_keys"}, nil
	}

	if pc.stsFake == nil {
		pc.stsFake = &probeSTS{}
	}
	h := &Handler{auth: mockAuth, config: store, credStore: &probeCredStore{payload: pc.payload, loadErr: pc.loadErr}}
	h.awsCfgOnce.Do(func() {
		h.awsCfg = aws.Config{
			Region:           "us-east-1",
			Credentials:      awscreds.NewStaticCredentialsProvider(probeHostKeyID, "host-secret", ""),
			HTTPClient:       pc.stsFake,
			RetryMaxAttempts: 1,
		}
		h.awsCfgErr = pc.hostCfgEr
	})
	req := &events.LambdaFunctionURLRequest{Headers: map[string]string{"Authorization": "Bearer admin-token"}}
	res, err := h.testAccountCredentials(ctx, req, probeAccountID)
	require.NoError(t, err)
	out, ok := res.(AccountTestResult)
	require.True(t, ok, "unexpected result type %T", res)
	return out
}

func assertNoLeak(t *testing.T, msg, allowedAcct string) {
	t.Helper()
	for _, m := range leakMarkers {
		assert.NotContains(t, msg, m, "response leaks %q", m)
	}
	for _, a := range []string{probeTenantAcct, probeOtherAcct} {
		if a != allowedAcct {
			assert.NotContains(t, msg, a, "response names an account other than the target")
		}
	}
}

func writeTokenFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(p, []byte("eyJ.token.sig"), 0o600))
	return p
}

// TestTestAccountCredentials_AWSFailuresAreClassified covers configs the old
// shape-only check reported OK for, plus probe failures, and asserts each one
// yields a fixed message with no host or driver detail.
func TestTestAccountCredentials_AWSFailuresAreClassified(t *testing.T) {
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", "")
	tests := []struct {
		name    string
		pc      probeCase
		wantMsg string
	}{
		{
			name:    "empty auth mode with stored access keys",
			pc:      probeCase{acct: &config.CloudAccount{ExternalID: probeTenantAcct}, payload: validAccessKeyPayload},
			wantMsg: "credential configuration incomplete: aws_auth_mode is not a supported mode",
		},
		{
			name:    "bastion mode without bastion id",
			pc:      probeCase{acct: &config.CloudAccount{ExternalID: probeTenantAcct, AWSAuthMode: "bastion", AWSRoleARN: probeTenantRole}},
			wantMsg: "credential configuration incomplete: aws_bastion_id is required",
		},
		{
			name:    "bastion mode without role ARN",
			pc:      probeCase{acct: &config.CloudAccount{ExternalID: probeTenantAcct, AWSAuthMode: "bastion", AWSBastionID: "b"}, payload: validAccessKeyPayload},
			wantMsg: "credential configuration incomplete: aws_role_arn is required",
		},
		{
			name:    "web identity without token file",
			pc:      probeCase{acct: &config.CloudAccount{ExternalID: probeTenantAcct, AWSAuthMode: "workload_identity_federation", AWSRoleARN: probeTenantRole}},
			wantMsg: "credential configuration incomplete: aws_web_identity_token_file is required",
		},
		{
			name:    "web identity with unreadable token file",
			pc:      probeCase{acct: &config.CloudAccount{ExternalID: probeTenantAcct, AWSAuthMode: "workload_identity_federation", AWSRoleARN: probeTenantRole, AWSWebIdentityTokenFile: "/nonexistent/probe/token"}},
			wantMsg: "web identity token file unreadable",
		},
		{
			name:    "role_arn without role ARN on a non-host account",
			pc:      probeCase{acct: &config.CloudAccount{ExternalID: probeTenantAcct, AWSAuthMode: "role_arn"}},
			wantMsg: "aws_role_arn is empty but this account is not the CUDly host account; set aws_role_arn",
		},
		{
			// #559: an STS outage is not the tenant's to fix, so it must not
			// be reported as a non-host account that needs aws_role_arn.
			name:    "role_arn without role ARN when the host identity check fails",
			pc:      probeCase{acct: &config.CloudAccount{ExternalID: probeHostAcct, AWSAuthMode: "role_arn"}, stsFake: &probeSTS{transportErr: true}},
			wantMsg: hostIdentityUnavailableMessage,
		},
		{
			name:    "access_keys with incomplete stored payload",
			pc:      probeCase{acct: &config.CloudAccount{ExternalID: probeTenantAcct, AWSAuthMode: "access_keys"}, payload: `{"access_key_id":"` + probeMemberKeyID + `"}`},
			wantMsg: "credential configuration incomplete: aws_access_keys stored payload is missing secret_access_key",
		},
		{
			name:    "access_keys not stored",
			pc:      probeCase{acct: &config.CloudAccount{ExternalID: probeTenantAcct, AWSAuthMode: "access_keys"}},
			wantMsg: "credentials not stored",
		},
		{
			name:    "credential store failure",
			pc:      probeCase{acct: &config.CloudAccount{ExternalID: probeTenantAcct, AWSAuthMode: "access_keys"}, loadErr: errors.New(probeDriverText)},
			wantMsg: "credential resolution failed (details in server logs)",
		},
		{
			name:    "host AWS config unavailable",
			pc:      probeCase{acct: &config.CloudAccount{ExternalID: probeTenantAcct, AWSAuthMode: "access_keys"}, payload: validAccessKeyPayload, hostCfgEr: errors.New(probeProfileText)},
			wantMsg: "CUDly host AWS configuration unavailable",
		},
		{
			name:    "role_arn whose AssumeRole is denied",
			pc:      probeCase{acct: &config.CloudAccount{ExternalID: probeTenantAcct, AWSAuthMode: "role_arn", AWSRoleARN: probeTenantRole}, stsFake: &probeSTS{denyAssumeRole: true}},
			wantMsg: "assume role denied for " + probeTenantRole,
		},
		{
			name:    "access keys rejected by GetCallerIdentity",
			pc:      probeCase{acct: &config.CloudAccount{ExternalID: probeTenantAcct, AWSAuthMode: "access_keys"}, payload: validAccessKeyPayload, stsFake: &probeSTS{identityErrCode: "InvalidClientTokenId"}},
			wantMsg: "credentials rejected by STS (InvalidClientTokenId)",
		},
		{
			name:    "STS unreachable",
			pc:      probeCase{acct: &config.CloudAccount{ExternalID: probeTenantAcct, AWSAuthMode: "access_keys"}, payload: validAccessKeyPayload, stsFake: &probeSTS{transportErr: true}},
			wantMsg: "STS unreachable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := runAccountTest(t, tt.pc)
			assert.False(t, res.OK, "test endpoint reported OK for a config real use rejects: %q", res.Message)
			assert.Equal(t, tt.wantMsg, res.Message)
			assertNoLeak(t, res.Message, probeTenantAcct)
		})
	}
}

// TestTestAccountCredentials_AWSAccountMismatch: credentials that work but
// belong to another AWS account than external_id must not report OK, and the
// response must not name the account they actually belong to.
func TestTestAccountCredentials_AWSAccountMismatch(t *testing.T) {
	fake := &probeSTS{}
	res := runAccountTest(t, probeCase{acct: &config.CloudAccount{ExternalID: probeOtherAcct, AWSAuthMode: "access_keys"}, payload: validAccessKeyPayload, stsFake: fake})
	assert.False(t, res.OK)
	assert.Equal(t, "credentials belong to a different AWS account than external_id "+probeOtherAcct, res.Message)
	assertNoLeak(t, res.Message, probeOtherAcct)
	assert.Equal(t, int32(1), fake.identityCalls.Load())
}

func TestTestAccountCredentials_AWSProbeTimeout(t *testing.T) {
	prev := awsAccountProbeTimeout
	awsAccountProbeTimeout = 50 * time.Millisecond
	t.Cleanup(func() { awsAccountProbeTimeout = prev })

	start := time.Now()
	res := runAccountTest(t, probeCase{acct: &config.CloudAccount{ExternalID: probeTenantAcct, AWSAuthMode: "access_keys"}, payload: validAccessKeyPayload, stsFake: &probeSTS{block: true}})
	assert.False(t, res.OK)
	assert.Equal(t, "STS probe timed out", res.Message)
	assert.Less(t, time.Since(start), 5*time.Second, "probe must honor its deadline")
}

func TestTestAccountCredentials_AWSValidConfigsProbeCallerIdentity(t *testing.T) {
	tokenFile := writeTokenFile(t)
	tests := []struct {
		name        string
		acct        *config.CloudAccount
		payload     string
		wantAcct    string
		wantAssume  int32
		wantWebAuth int32
		wantSigners []string
	}{
		{
			name:     "access_keys",
			acct:     &config.CloudAccount{ExternalID: probeTenantAcct, AWSAuthMode: "access_keys"},
			payload:  validAccessKeyPayload,
			wantAcct: probeTenantAcct,
		},
		{
			name:       "role_arn with role ARN",
			acct:       &config.CloudAccount{ExternalID: probeTenantAcct, AWSAuthMode: "role_arn", AWSRoleARN: probeTenantRole},
			wantAcct:   probeTenantAcct,
			wantAssume: 1,
		},
		{
			name:     "role_arn without role ARN on the host account",
			acct:     &config.CloudAccount{ExternalID: probeHostAcct, AWSAuthMode: "role_arn"},
			wantAcct: probeHostAcct,
		},
		{
			name:        "workload_identity_federation with a token file",
			acct:        &config.CloudAccount{ExternalID: probeTenantAcct, AWSAuthMode: "workload_identity_federation", AWSRoleARN: probeTenantRole, AWSWebIdentityTokenFile: tokenFile},
			wantAcct:    probeTenantAcct,
			wantWebAuth: 1,
		},
		{
			// Issue #263: the target role is assumed with the bastion's own
			// keys (the stored payload), never the host's.
			name:        "bastion with bastion id and role ARN",
			acct:        &config.CloudAccount{ExternalID: probeTenantAcct, AWSAuthMode: "bastion", AWSBastionID: "bastion-1", AWSRoleARN: probeTenantRole},
			payload:     validAccessKeyPayload,
			wantAcct:    probeTenantAcct,
			wantAssume:  1,
			wantSigners: []string{probeMemberKeyID},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &probeSTS{}
			res := runAccountTest(t, probeCase{acct: tt.acct, payload: tt.payload, stsFake: fake})
			assert.True(t, res.OK, "unexpected failure: %q", res.Message)
			assert.Equal(t, "credentials validated (sts:GetCallerIdentity succeeded for AWS account "+tt.wantAcct+")", res.Message)
			assert.NotContains(t, res.Message, "arn:", "success must report the target account, not the caller ARN")
			assert.Equal(t, tt.wantAssume, fake.assumeRoles.Load())
			assert.Equal(t, tt.wantWebAuth, fake.webIdentityCalls.Load())
			if tt.wantSigners != nil {
				assert.Equal(t, tt.wantSigners, fake.assumeRoleSigners(), "AssumeRole signed by the wrong credentials")
			}
			assert.GreaterOrEqual(t, fake.identityCalls.Load(), int32(1), "sts:GetCallerIdentity probe must run")
		})
	}
}
