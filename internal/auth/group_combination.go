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
	viewOnly, writeCapable, err := splitGroupCombination(ctx, groupIDs, s.combinationGroup)
	if err != nil {
		return err
	}
	if len(viewOnly) == 0 || len(writeCapable) == 0 {
		return nil
	}
	return contradictoryGroupsError(viewOnly, writeCapable)
}

// groupLoader loads one group for the combination rule. It returns nil, nil
// for a group that does not exist.
type groupLoader func(ctx context.Context, id string) (*Group, error)

// combinationGroup is the store-backed groupLoader: a missing group is nil,
// any other store error fails closed.
func (s *Service) combinationGroup(ctx context.Context, id string) (*Group, error) {
	group, err := s.store.GetGroup(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to load group %s: %w", id, err)
	}
	return group, nil
}

// splitGroupCombination returns the names of the view-only and the
// write-capable groups among groupIDs. Fewer than two distinct groups cannot
// contradict, so it loads nothing for them.
func splitGroupCombination(ctx context.Context, groupIDs []string, load groupLoader) (viewOnly, writeCapable []string, err error) {
	ids := uniqueGroupIDs(groupIDs)
	if len(ids) < 2 {
		// A contradiction needs two different groups.
		return nil, nil, nil
	}
	for _, id := range ids {
		group, loadErr := load(ctx, id)
		if loadErr != nil {
			return nil, nil, loadErr
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
	return viewOnly, writeCapable, nil
}

// groupsContradict reports whether groupIDs hold both a view-only and a
// write-capable group.
func groupsContradict(ctx context.Context, groupIDs []string, load groupLoader) (bool, error) {
	viewOnly, writeCapable, err := splitGroupCombination(ctx, groupIDs, load)
	return len(viewOnly) > 0 && len(writeCapable) > 0, err
}

// combinationRole is the part a group plays in the combination rule.
type combinationRole int

const (
	roleNeutral combinationRole = iota
	roleViewOnly
	roleWriteCapable
)

func groupCombinationRole(g *Group) combinationRole {
	switch {
	case groupIsViewOnly(g):
		return roleViewOnly
	case groupIsWriteCapable(g):
		return roleWriteCapable
	}
	return roleNeutral
}

// checkGroupPermissionChange applies the combination rule to a group update
// (issue #1409). Changing a group's permissions changes the combination of
// every member, with no user write: adding a write permission to a group
// whose members also hold a view-only group contradicts each of them, and so
// does making a group view-only while its members also hold a write-capable
// one.
//
// Only the group's role in the rule matters, so an update that keeps it
// view-only, write-capable or neutral returns at once without reading any
// user. Otherwise the update is refused when it would make at least one
// member contradictory who was not before. A member who was already
// contradictory (assigned before #1205, or by an earlier group edit) does not
// block it, as an unchanged membership does not block a user edit.
//
// Like checkGroupCombination it is an accident guard, not a security
// boundary, and it fails closed on a store error.
func (s *Service) checkGroupPermissionChange(ctx context.Context, prior, next *Group) error {
	if groupCombinationRole(prior) == groupCombinationRole(next) {
		return nil
	}
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return fmt.Errorf("failed to list users for the group combination check: %w", err)
	}
	stored := cachedGroupLoader(s.combinationGroup)
	updated := func(ctx context.Context, id string) (*Group, error) {
		if id == next.ID {
			return next, nil
		}
		return stored(ctx, id)
	}
	affected, err := countNewlyContradictory(ctx, users, next.ID, stored, updated)
	if err != nil {
		return err
	}
	if affected == 0 {
		return nil
	}
	return groupChangeContradictionError(next, affected)
}

// countNewlyContradictory counts the members of groupID whose groups do not
// contradict when loaded with before but do when loaded with after.
func countNewlyContradictory(ctx context.Context, users []User, groupID string, before, after groupLoader) (int, error) {
	affected := 0
	for i := range users {
		ids := users[i].GroupIDs
		if !containsGroup(ids, groupID) {
			continue
		}
		was, err := groupsContradict(ctx, ids, before)
		if err != nil {
			return 0, err
		}
		now, err := groupsContradict(ctx, ids, after)
		if err != nil {
			return 0, err
		}
		if now && !was {
			affected++
		}
	}
	return affected, nil
}

// cachedGroupLoader loads each group at most once per check: every member of
// the updated group shares it, and usually most of their other groups.
func cachedGroupLoader(load groupLoader) groupLoader {
	cache := map[string]*Group{}
	return func(ctx context.Context, id string) (*Group, error) {
		if g, ok := cache[id]; ok {
			return g, nil
		}
		g, err := load(ctx, id)
		if err != nil {
			return nil, err
		}
		cache[id] = g
		return g, nil
	}
}

// groupChangeContradictionError explains a refused group update in the terms
// of the change: what the group would become and how many members conflict.
func groupChangeContradictionError(next *Group, affected int) error {
	members, belong := "1 of its members", "belongs"
	if affected > 1 {
		members, belong = fmt.Sprintf("%d of its members", affected), "belong"
	}
	becomes, other := "would become view-only", "write-capable"
	if groupCombinationRole(next) == roleWriteCapable {
		becomes, other = "would grant write permissions", "view-only"
	}
	return fmt.Errorf("%w: group %q %s, but %s also %s to a %s group; remove them from one of the two groups first",
		ErrContradictoryGroups, next.Name, becomes, members, belong, other)
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
