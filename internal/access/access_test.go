package access

import (
	"testing"

	"termdock/internal/model"
)

func TestEffectiveRole(t *testing.T) {
	tests := []struct {
		name      string
		shareRole string
		orgRole   string
		wantRole  string
		wantOK    bool
	}{
		{"no access", "", "", "", false},
		{"org member becomes collaborator", "", model.OrgRoleMember, model.ShareRoleCollaborator, true},
		{"org admin becomes collaborator", "", model.OrgRoleAdmin, model.ShareRoleCollaborator, true},
		{"org owner becomes collaborator", "", model.OrgRoleOwner, model.ShareRoleCollaborator, true},
		{"share viewer for outsider", model.ShareRoleViewer, "", model.ShareRoleViewer, true},
		{"share collaborator for outsider", model.ShareRoleCollaborator, "", model.ShareRoleCollaborator, true},
		{"share overrides org membership (downgrade to viewer)", model.ShareRoleViewer, model.OrgRoleMember, model.ShareRoleViewer, true},
		{"share overrides org admin", model.ShareRoleViewer, model.OrgRoleAdmin, model.ShareRoleViewer, true},
		{"share can upgrade to collaborator explicitly", model.ShareRoleCollaborator, model.OrgRoleMember, model.ShareRoleCollaborator, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			role, ok := EffectiveRole(tt.shareRole, tt.orgRole)
			if role != tt.wantRole || ok != tt.wantOK {
				t.Fatalf("EffectiveRole(%q,%q) = (%q,%v), want (%q,%v)",
					tt.shareRole, tt.orgRole, role, ok, tt.wantRole, tt.wantOK)
			}
		})
	}
}

func TestCanWrite(t *testing.T) {
	if !CanWrite(model.ShareRoleCollaborator) {
		t.Fatal("collaborator should be able to write")
	}
	if CanWrite(model.ShareRoleViewer) {
		t.Fatal("viewer must not be able to write")
	}
	if CanWrite("") {
		t.Fatal("empty role must not be able to write")
	}
}

func TestCanControl(t *testing.T) {
	if !CanControl(model.ShareRoleCollaborator, false, "") {
		t.Fatal("collaborator share should control")
	}
	if CanControl(model.ShareRoleViewer, false, "") {
		t.Fatal("viewer share must not control")
	}
	if CanControl(model.ShareRoleViewer, false, model.OrgRoleMember) {
		t.Fatal("org member downgraded to viewer via share must lose control (share overrides org default)")
	}
	if !CanControl("", true, "") {
		t.Fatal("creator should control")
	}
	if !CanControl(model.ShareRoleViewer, false, model.OrgRoleAdmin) {
		t.Fatal("org admin should control")
	}
	if CanControl("", false, "") {
		t.Fatal("stranger must not control")
	}
}

func TestCanDelete(t *testing.T) {
	if !CanDelete(true, "") {
		t.Fatal("creator should delete")
	}
	if !CanDelete(false, model.OrgRoleOwner) {
		t.Fatal("owner should delete")
	}
	if !CanDelete(false, model.OrgRoleAdmin) {
		t.Fatal("admin should delete")
	}
	if CanDelete(false, model.OrgRoleMember) {
		t.Fatal("plain member must not delete")
	}
	if CanDelete(false, "") {
		t.Fatal("outsider must not delete")
	}
}

func TestCanManageMembers(t *testing.T) {
	if !CanManageMembers(model.OrgRoleOwner) || !CanManageMembers(model.OrgRoleAdmin) {
		t.Fatal("owner/admin should manage members")
	}
	if CanManageMembers(model.OrgRoleMember) || CanManageMembers("") {
		t.Fatal("member/outsider must not manage members")
	}
}

func TestCanRemoveMember(t *testing.T) {
	if !CanRemoveMember(model.OrgRoleOwner, model.OrgRoleMember, 1) {
		t.Fatal("owner should remove a member")
	}
	if CanRemoveMember(model.OrgRoleMember, model.OrgRoleMember, 1) {
		t.Fatal("member must not remove anyone")
	}
	if CanRemoveMember(model.OrgRoleOwner, model.OrgRoleOwner, 1) {
		t.Fatal("last owner must never be removable")
	}
	if !CanRemoveMember(model.OrgRoleOwner, model.OrgRoleOwner, 2) {
		t.Fatal("owner should be removable when another owner remains")
	}
	if !CanRemoveMember(model.OrgRoleAdmin, model.OrgRoleOwner, 2) {
		t.Fatal("admin should remove a non-last owner")
	}
}

func TestValidRoles(t *testing.T) {
	for _, r := range []string{"owner", "admin", "member"} {
		if !ValidOrgRole(r) {
			t.Fatalf("ValidOrgRole(%q) = false", r)
		}
	}
	if ValidOrgRole("superuser") {
		t.Fatal("bogus org role accepted")
	}
	for _, r := range []string{"viewer", "collaborator"} {
		if !ValidShareRole(r) {
			t.Fatalf("ValidShareRole(%q) = false", r)
		}
	}
	if ValidShareRole("owner") {
		t.Fatal("org role accepted as share role")
	}
}
