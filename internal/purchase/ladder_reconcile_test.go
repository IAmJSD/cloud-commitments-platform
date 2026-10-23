package purchase

import (
	"errors"
	"testing"

	"github.com/LeanerCloud/cloud-commitments-go/pkg/ladder"
	"github.com/LeanerCloud/cloud-commitments-platform/internal/config"
	"github.com/stretchr/testify/assert"
)

func chainNode(id, status string, proof bool) config.LadderChainNode {
	return config.LadderChainNode{ExecutionID: id, Status: status, NoSubmission: proof}
}

func visibleAlways(string) (bool, error) { return true, nil }

// Pure mapping (no DB). The real-database behavior is in
// ladder_reconcile_integration_test.go.
func TestDecideFiredTranche(t *testing.T) {
	linked := chainNode("e1", "failed", true)
	next := "e2"
	linked.RetryExecutionID = &next
	cases := []struct {
		name    string
		chain   []config.LadderChainNode
		visible func(string) (bool, error)
		want    ladder.TrancheStatus
		reason  string
	}{
		{"failed with proof", []config.LadderChainNode{chainNode("e1", "failed", true)}, visibleAlways, ladder.TrancheStatusFailed, "no_submission"},
		{"failed without proof (reaped or outcome unknown)", []config.LadderChainNode{chainNode("e1", "failed", false)}, visibleAlways, "", ""},
		{"first attempt reaped, later attempts proven", []config.LadderChainNode{chainNode("e1", "failed", false), chainNode("e2", "failed", true), chainNode("e3", "failed", true)}, visibleAlways, "", ""},
		{"every attempt proven", []config.LadderChainNode{chainNode("e1", "failed", true), chainNode("e2", "failed", true)}, visibleAlways, ladder.TrancheStatusFailed, "no_submission"},
		{"canceled tail", []config.LadderChainNode{chainNode("e1", "canceled", false)}, visibleAlways, ladder.TrancheStatusFailed, "approval_canceled"},
		{"expired tail after unproven attempt", []config.LadderChainNode{chainNode("e1", "failed", false), chainNode("e2", "expired", false)}, visibleAlways, "", ""},
		{"proof flag ignored on a non-failed row", []config.LadderChainNode{chainNode("e1", "running", true)}, visibleAlways, "", ""},
		{"partially completed", []config.LadderChainNode{chainNode("e1", "partially_completed", false)}, visibleAlways, "", ""},
		{"completed and visible", []config.LadderChainNode{chainNode("e1", "completed", false)}, visibleAlways, ladder.TrancheStatusCompleted, "commitment_visible"},
		{"completed not visible", []config.LadderChainNode{chainNode("e1", "completed", false)}, func(string) (bool, error) { return false, nil }, "", ""},
		{"completed visibility error", []config.LadderChainNode{chainNode("e1", "completed", false)}, func(string) (bool, error) { return false, errors.New("boom") }, "", ""},
		{"chain cut short (tail still linked)", []config.LadderChainNode{linked}, visibleAlways, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := decideFiredTranche(c.chain, c.visible)
			assert.Equal(t, c.want, v.to)
			assert.Equal(t, c.reason, v.reason)
			if c.want == "" {
				assert.NotEmpty(t, v.note, "a tranche left fired must say why")
			}
		})
	}
}
