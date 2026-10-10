package db

import (
	"testing"

	"termdock/internal/model"
)

// Regression test: ListVisibleSessions with no org filter must return the
// user's org sessions, not just direct shares. (A bare `s.org_id = ?` with
// an empty orgID matched nothing, so the dashboard listed zero sessions and
// newly created terminals never opened a tab.)
func TestListVisibleSessionsNoOrgFilter(t *testing.T) {
	d, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	for _, u := range []struct{ id, name string }{{"u1", "alice"}, {"u9", "bob"}} {
		if err := d.CreateUser(&model.User{ID: u.id, Username: u.name, PasswordHash: "x", CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}

	mkSession := func(id, orgID, name string) {
		if err := d.CreateSession(&model.Session{
			ID: id, Name: name, OrgID: orgID, Image: "ubuntu:latest",
			Shell: "/bin/bash", Status: model.SessionRunning,
			CreatedBy: "u1", CreatedAt: 1, LastActiveAt: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// u1 belongs to org-a only.
	for _, o := range []struct{ id, name string }{{"org-a", "A"}, {"org-b", "B"}} {
		if err := d.CreateOrg(&model.Org{ID: o.id, Name: o.name, CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.AddOrgMember("org-a", "u1", "owner", 1); err != nil {
		t.Fatal(err)
	}
	mkSession("s1", "org-a", "mine")
	mkSession("s2", "org-b", "theirs")

	// No org filter: must see the org-a session, not the org-b one.
	got, err := d.ListVisibleSessions("u1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "s1" {
		t.Fatalf("unfiltered list = %v, want [s1]", ids(got))
	}

	// Org filter: only that org's sessions.
	got, err = d.ListVisibleSessions("u1", "org-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "s1" {
		t.Fatalf("org-a list = %v, want [s1]", ids(got))
	}
	got, err = d.ListVisibleSessions("u1", "org-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("org-b list = %v, want []", ids(got))
	}

	// A direct share makes an out-of-org session visible even unfiltered.
	if err := d.CreateShare(&model.Share{
		ID: "sh1", SessionID: "s2", UserID: "u1",
		Role: "viewer", CreatedBy: "u9", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	got, err = d.ListVisibleSessions("u1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("unfiltered list with share = %v, want [s1 s2]", ids(got))
	}
}

func ids(ss []model.Session) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.ID
	}
	return out
}
