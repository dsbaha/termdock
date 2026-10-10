// Package access contains pure authorization logic: resolving a user's
// effective role on a session and answering can-do checks. It is deliberately
// free of I/O so it can be unit tested.
package access

import "termdock/internal/model"

// EffectiveRole resolves what role a user has on a session.
//
//   - shareRole: the user's explicit share grant role ("" if none). An
//     explicit share ALWAYS wins, even over org membership — this is what
//     allows downgrading an org member to view-only on a specific session.
//   - orgRole: the user's role in the session's org ("" if not a member).
//
// An org member with no share grant is a collaborator (full interactive).
func EffectiveRole(shareRole, orgRole string) (role string, ok bool) {
	if shareRole != "" {
		return shareRole, true
	}
	if orgRole != "" {
		return model.ShareRoleCollaborator, true
	}
	return "", false
}

// CanWrite reports whether the effective role may send terminal input.
func CanWrite(effRole string) bool {
	return effRole == model.ShareRoleCollaborator
}

// CanControl reports whether the user may start/stop a session.
// Collaborators (via org or share), the creator, and org owners/admins may.
func CanControl(effRole string, isCreator bool, orgRole string) bool {
	if isCreator || orgRole == model.OrgRoleOwner || orgRole == model.OrgRoleAdmin {
		return true
	}
	return effRole == model.ShareRoleCollaborator
}

// CanDelete reports whether the user may delete a session.
// Only the creator and org owners/admins may (shared users never).
func CanDelete(isCreator bool, orgRole string) bool {
	return isCreator || orgRole == model.OrgRoleOwner || orgRole == model.OrgRoleAdmin
}

// CanShare reports whether the user may manage share grants on a session.
func CanShare(isCreator bool, orgRole string) bool {
	return isCreator || orgRole == model.OrgRoleOwner || orgRole == model.OrgRoleAdmin
}

// CanManageMembers reports whether the org role may add/remove members.
func CanManageMembers(orgRole string) bool {
	return orgRole == model.OrgRoleOwner || orgRole == model.OrgRoleAdmin
}

// ValidOrgRole reports whether r is a known org role.
func ValidOrgRole(r string) bool {
	return r == model.OrgRoleOwner || r == model.OrgRoleAdmin || r == model.OrgRoleMember
}

// ValidShareRole reports whether r is a known share role.
func ValidShareRole(r string) bool {
	return r == model.ShareRoleViewer || r == model.ShareRoleCollaborator
}

// CanRemoveMember decides whether actorRole may remove a member with
// targetRole, given ownerCount owners remain in the org.
// The last owner can never be removed (not even by themselves).
func CanRemoveMember(actorRole, targetRole string, ownerCount int) bool {
	if !CanManageMembers(actorRole) {
		return false
	}
	if targetRole == model.OrgRoleOwner && ownerCount <= 1 {
		return false
	}
	return true
}
