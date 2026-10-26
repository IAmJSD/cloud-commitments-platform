package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Group-combination guard (issue #78).
//
// A view-only group (every permission's action is "view") combined with a
// write-capable group (at least one permission with any other action) is a
// contradiction: permissions are a plain union, so the write group silently
// overrides the read-only intent of the view-only one. The frontend's
// validateGroupCombination (frontend/src/users/utils.ts) has refused this
// since #1405, but nothing stopped a non-UI API consumer.
//
// This is a port of that function, and the classification must stay exactly
// the same:
//
//   - It reads each group's stored permissions, never its name.
//   - view-only:     at least one permission, all with action == "view".
//   - write-capable: at least one permission whose action != "view"
//     (so "*" and "admin" count, and a mixed view+create group counts).
//   - A group with no permissions is neither and does not take part.
//   - A group that does not exist (deleted, or never existed) is skipped.
//
// It is an accident guard, not a security boundary: an update:users holder
// could assign the write group alone anyway.

// groupIsViewOnly reports whether every one of g's permissions is a view,
// with at least one permission present.
func groupIsViewOnly(g *Group) bool {
	if len(g.Permissions) == 0 {
		return false
	}
	for _, p := range g.Permissions {
		if p.Action != ActionView {
			return false
		}
	}
	return true
}

// groupIsWriteCapable reports whether any of g's permissions is not a view.
func groupIsWriteCapable(g *Group) bool {
	for _, p := range g.Permissions {
		if p.Action != ActionView {
			return true
		}
	}
	return false
}

// checkGroupMembershipChange runs checkGroupCombination only when the
// membership actually changes. A user who already holds a contradictory
// membership (assigned before this check existed, or made contradictory by a
// later edit to one of the groups) must still be renamed, deactivated or
// saved by a client that echoes back the unchanged group list. Any real change
// is checked against the full resulting set, as the frontend does, so it has
// to resolve the contradiction rather than add to it.
func (s *Service) checkGroupMembershipChange(ctx context.Context, prior, next []string) error {
	if sameGroupSet(prior, next) {
		return nil
	}
	return s.checkGroupCombination(ctx, next)
}

// checkGroupCombination returns an ErrContradictoryGroups error when groupIDs
// contains both a view-only group and a write-capable group. It fails closed
// on a store error other than "not found".
func (s *Service) checkGroupCombination(ctx context.Context, groupIDs []string) error {
	ids := uniqueGroupIDs(groupIDs)
	if len(ids) < 2 {
		// A contradiction needs two different groups.
		return nil
	}
	var viewOnly, writeCapable []string
	for _, id := range ids {
		group, err := s.store.GetGroup(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("failed to load group %s: %w", id, err)
		}
		if group == nil {
			continue
		}
		switch {
		case groupIsViewOnly(group):
			viewOnly = append(viewOnly, group.Name)
		case groupIsWriteCapable(group):
			writeCapable = append(writeCapable, group.Name)
		}
	}
	if len(viewOnly) == 0 || len(writeCapable) == 0 {
		return nil
	}
	return contradictoryGroupsError(viewOnly, writeCapable)
}

// contradictoryGroupsError builds the same message the frontend shows, so an
// API consumer and a UI user see the same explanation.
func contradictoryGroupsError(viewOnly, writeCapable []string) error {
	is, grants := "is", "grants"
	if len(viewOnly) > 1 {
		is = "are"
	}
	if len(writeCapable) > 1 {
		grants = "grant"
	}
	return fmt.Errorf("%w: %s %s view-only but %s %s write permissions; assign a view-only group or write-capable groups, not both",
		ErrContradictoryGroups, quoteNames(viewOnly), is, quoteNames(writeCapable), grants)
}

func quoteNames(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	return strings.Join(quoted, ", ")
}

// uniqueGroupIDs returns ids without duplicates, keeping first-seen order so
// the error names groups in the order the caller sent them.
func uniqueGroupIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !containsGroup(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// sameGroupSet reports whether a and b name the same groups, ignoring order
// and duplicates.
func sameGroupSet(a, b []string) bool {
	return len(addedGroups(a, b)) == 0 && len(addedGroups(b, a)) == 0
}
