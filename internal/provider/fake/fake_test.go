package fake

import (
	"context"
	"errors"
	"testing"

	"github.com/backhand/ecu/internal/provider"
)

// TestFakeRecordsCreateAndDelete verifies the fake records creates (with their
// UserData) and deletes, hands out incrementing instance ids, and that
// DeleteInstance is idempotent.
func TestFakeRecordsCreateAndDelete(t *testing.T) {
	p := New()
	ctx := context.Background()

	if p.CreateCount() != 0 || p.DeleteCount() != 0 {
		t.Fatalf("fresh fake should have zero counts")
	}

	inst1, err := p.CreateInstance(ctx, provider.InstanceSpec{UserData: "#cloud-config\n# one"})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	inst2, err := p.CreateInstance(ctx, provider.InstanceSpec{UserData: "#cloud-config\n# two"})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if inst1.ID == inst2.ID {
		t.Fatalf("instance ids must increment, got %q twice", inst1.ID)
	}
	if inst1.ID != "fake-1" || inst2.ID != "fake-2" {
		t.Fatalf("ids = %q,%q want fake-1,fake-2", inst1.ID, inst2.ID)
	}
	if inst1.PublicIP == "" || inst2.PublicIP == "" {
		t.Fatalf("instances should have a fake public IP")
	}
	if p.CreateCount() != 2 {
		t.Fatalf("CreateCount = %d, want 2", p.CreateCount())
	}
	if p.LastUserData() != "#cloud-config\n# two" {
		t.Fatalf("LastUserData = %q, want the most recent spec's UserData", p.LastUserData())
	}
	if got := p.Instances(); len(got) != 2 {
		t.Fatalf("Instances len = %d, want 2", len(got))
	}

	// Delete the first; idempotent on repeat and on unknown id.
	if err := p.DeleteInstance(ctx, inst1.ID); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if !p.Deleted(inst1.ID) {
		t.Fatalf("Deleted(%q) = false, want true", inst1.ID)
	}
	if err := p.DeleteInstance(ctx, inst1.ID); err != nil {
		t.Fatalf("repeat DeleteInstance must be idempotent (nil), got %v", err)
	}
	if err := p.DeleteInstance(ctx, "fake-does-not-exist"); err != nil {
		t.Fatalf("DeleteInstance(unknown) must be idempotent (nil), got %v", err)
	}
	if got := p.Instances(); len(got) != 1 {
		t.Fatalf("after delete Instances len = %d, want 1", len(got))
	}
}

// TestFakeCreateErr verifies CreateErr forces a failure and records nothing.
func TestFakeCreateErr(t *testing.T) {
	p := New()
	sentinel := errors.New("boom")
	p.CreateErr = sentinel

	_, err := p.CreateInstance(context.Background(), provider.InstanceSpec{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("CreateInstance err = %v, want sentinel", err)
	}
	if p.CreateCount() != 0 {
		t.Fatalf("failed create must not be recorded; CreateCount = %d", p.CreateCount())
	}
}

// TestFakeOnCreateHook verifies the OnCreate hook fires with the spec.
func TestFakeOnCreateHook(t *testing.T) {
	p := New()
	var gotUserData string
	p.OnCreate = func(spec provider.InstanceSpec) { gotUserData = spec.UserData }

	if _, err := p.CreateInstance(context.Background(), provider.InstanceSpec{UserData: "hello"}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if gotUserData != "hello" {
		t.Fatalf("OnCreate got UserData %q, want %q", gotUserData, "hello")
	}
}

// TestFakeStubs exercises the stubbed interface methods and their C7 recording.
func TestFakeStubs(t *testing.T) {
	p := New()
	ctx := context.Background()

	img, err := p.CreateImage(ctx, "fake-1", "my-image")
	if err != nil || img.Name != "my-image" {
		t.Fatalf("CreateImage = %+v, %v", img, err)
	}
	if p.ImageCount() != 1 {
		t.Fatalf("ImageCount = %d, want 1", p.ImageCount())
	}
	if calls := p.Images(); len(calls) != 1 || calls[0].FromInstance != "fake-1" || calls[0].Name != "my-image" {
		t.Fatalf("Images() = %+v, want one call {fake-1, my-image}", calls)
	}
	// FindImage defaults to not-found.
	if _, found, err := p.FindImage(ctx, "my-image"); err != nil || found {
		t.Fatalf("FindImage should default to not found, got found=%v err=%v", found, err)
	}
	if err := p.EnsureFirewall(ctx, nil); err != nil {
		t.Fatalf("EnsureFirewall: %v", err)
	}
}

// TestFakeFindImageResult verifies the FindImageResult hook is honored, so
// startup tests can model a pre-existing snapshot.
func TestFakeFindImageResult(t *testing.T) {
	p := New()
	p.FindImageResult = &FindImageResult{Image: provider.Image{ID: "img-123", Name: "ecu-snap"}, Found: true}
	img, found, err := p.FindImage(context.Background(), "ecu-snap")
	if err != nil || !found {
		t.Fatalf("FindImage = (%+v, %v, %v), want found", img, found, err)
	}
	if img.ID != "img-123" {
		t.Fatalf("image id = %q, want img-123", img.ID)
	}
}

// TestFakeCreateImageErr verifies CreateImageErr forces a failure (the bake
// snapshot-failure path) and records nothing.
func TestFakeCreateImageErr(t *testing.T) {
	p := New()
	p.CreateImageErr = errors.New("snapshot boom")
	if _, err := p.CreateImage(context.Background(), "fake-1", "x"); err == nil {
		t.Fatalf("CreateImage should fail when CreateImageErr is set")
	}
	if p.ImageCount() != 0 {
		t.Fatalf("a failed CreateImage must record nothing; ImageCount = %d", p.ImageCount())
	}
}

// TestFakeDeleteInstancesByLabel verifies label-selected deletion matches the
// spec labels a real provider would, and is idempotent when nothing matches.
func TestFakeDeleteInstancesByLabel(t *testing.T) {
	p := New()
	ctx := context.Background()

	bake, _ := p.CreateInstance(ctx, provider.InstanceSpec{Labels: map[string]string{"ecu-bake": "1"}})
	other, _ := p.CreateInstance(ctx, provider.InstanceSpec{Labels: map[string]string{"ecu-session": "s_x"}})

	n, err := p.DeleteInstancesByLabel(ctx, "ecu-bake", "1")
	if err != nil || n != 1 {
		t.Fatalf("DeleteInstancesByLabel = (%d, %v), want (1, nil)", n, err)
	}
	if !p.Deleted(bake.ID) {
		t.Fatalf("bake instance %s was not deleted by label", bake.ID)
	}
	if p.Deleted(other.ID) {
		t.Fatalf("non-matching instance %s must NOT be deleted", other.ID)
	}
	// Idempotent: a second sweep matches nothing now.
	if n, err := p.DeleteInstancesByLabel(ctx, "ecu-bake", "1"); err != nil || n != 0 {
		t.Fatalf("second DeleteInstancesByLabel = (%d, %v), want (0, nil)", n, err)
	}
}

// compile-time check that *Provider satisfies provider.Provider.
var _ provider.Provider = (*Provider)(nil)
