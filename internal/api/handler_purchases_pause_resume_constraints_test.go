package api

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/LeanerCloud/cloud-commitments-platform/internal/auth"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
)

// Issue #1098: pause/resume let an update-any holder act on any scheduled
// purchase without evaluating that permission's Constraints. The cancelRouteExec
// row is azure/compute/westeurope on account "acct-1".

const pauseResumeOtherUser = "someone-else"

type pauseResumeOp struct {
	name string
	run  func(h *Handler) (*StatusResponse, error)
	// status is the row status the op starts from.
	status string
}

var pauseResumeOps = []pauseResumeOp{
	{"pause", func(h *Handler) (*StatusResponse, error) {
		return h.pausePlannedPurchase(context.Background(), deletePlannedReq(), cancelExecID)
	}, "pending"},
	{"resume", func(h *Handler) (*StatusResponse, error) {
		return h.resumePlannedPurchase(context.Background(), deletePlannedReq(), cancelExecID)
	}, "paused"},
}

func runPauseResume(op pauseResumeOp, creator *string, recs []config.RecommendationRecord, perms ...auth.Permission) (*StatusResponse, *MockConfigStore, error) {
	exec := cancelRouteExec(op.status)
	exec.CreatedByUserID = creator
	exec.ScheduledDate = time.Now().Add(24 * time.Hour)
	if recs != nil {
		exec.Recommendations = recs
	}
	h, store := cancelRouteHandler(exec, append(perms, revokePerm(auth.ActionUpdate, nil))...)
	resp, err := op.run(h)
	return resp, store, err
}

func requireNotTransitioned(t *testing.T, store *MockConfigStore) {
	t.Helper()
	store.AssertNotCalled(t, "TransitionExecutionStatus", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

func TestPauseResumePlanned_UpdateAnyConstraintDimensions(t *testing.T) {
	t.Parallel()
	other := pauseResumeOtherUser
	denying := map[string]*auth.PermissionConstraints{
		"providers":   {Providers: []string{"aws"}},
		"services":    {Services: []string{"rds"}},
		"regions":     {Regions: []string{"eastus"}},
		"account ids": {AccountIDs: []string{"other-acct"}},
	}
	allowing := map[string]*auth.PermissionConstraints{
		"providers":   {Providers: []string{"azure"}},
		"services":    {Services: []string{"compute"}},
		"regions":     {Regions: []string{"westeurope"}},
		"account ids": {AccountIDs: []string{"acct-1"}},
		"none":        nil,
	}
	for _, op := range pauseResumeOps {
		for dim, c := range denying {
			t.Run(op.name+"/deny/"+dim, func(t *testing.T) {
				t.Parallel()
				_, store, err := runPauseResume(op, &other, nil, revokePerm(auth.ActionUpdateAny, c))
				requireRevokeForbidden(t, err, constraintDenied)
				requireNotTransitioned(t, store)
			})
		}
		for dim, c := range allowing {
			t.Run(op.name+"/allow/"+dim, func(t *testing.T) {
				t.Parallel()
				resp, _, err := runPauseResume(op, &other, nil, revokePerm(auth.ActionUpdateAny, c))
				require.NoError(t, err)
				require.NotNil(t, resp)
			})
		}
	}
}

func TestPauseResumePlanned_UpdateAnyStrictScope(t *testing.T) {
	t.Parallel()
	other := pauseResumeOtherUser
	azureOnly := revokePerm(auth.ActionUpdateAny, &auth.PermissionConstraints{Providers: []string{"azure"}})
	for _, op := range pauseResumeOps {
		t.Run(op.name+"/every recommendation must be in scope", func(t *testing.T) {
			t.Parallel()
			recs := []config.RecommendationRecord{azureRec(), recFor("acct-uuid", "aws", "ec2", "us-east-1")}
			_, store, err := runPauseResume(op, &other, recs, azureOnly)
			requireRevokeForbidden(t, err, constraintDenied)
			requireNotTransitioned(t, store)
		})
		t.Run(op.name+"/no recommendations is refused by a bounded grant", func(t *testing.T) {
			t.Parallel()
			_, store, err := runPauseResume(op, &other, []config.RecommendationRecord{}, azureOnly)
			requireRevokeForbidden(t, err, constraintDenied)
			requireNotTransitioned(t, store)
		})
		t.Run(op.name+"/admin is unaffected", func(t *testing.T) {
			t.Parallel()
			resp, _, err := runPauseResume(op, &other, nil, auth.Permission{Action: auth.ActionAdmin, Resource: auth.ResourceAll})
			require.NoError(t, err)
			assert.NotNil(t, resp)
		})
	}
}

// A bounded update-any denial still lets the row's creator manage their own
// scheduled purchase, and keeps the constraint message for anyone else.
func TestPauseResumePlanned_UpdateAnyDenialFallsBackToCreatorOnly(t *testing.T) {
	t.Parallel()
	creator, other := revokeConstraintsUser, pauseResumeOtherUser
	updateOut := awsOnly(auth.ActionUpdateAny)
	for _, op := range pauseResumeOps {
		t.Run(op.name+"/creator allowed", func(t *testing.T) {
			t.Parallel()
			resp, _, err := runPauseResume(op, &creator, nil, updateOut)
			require.NoError(t, err)
			assert.NotNil(t, resp)
		})
		t.Run(op.name+"/non-creator denied", func(t *testing.T) {
			t.Parallel()
			_, store, err := runPauseResume(op, &other, nil, updateOut)
			requireRevokeForbidden(t, err, constraintDenied)
			requireNotTransitioned(t, store)
		})
	}
}
