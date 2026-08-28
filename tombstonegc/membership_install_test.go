package tombstonegc

import (
	"errors"
	"testing"

	"github.com/im10furry/crdt"
)

func TestCoordinatorInstallsAuthoritativeMembershipEpoch(t *testing.T) {
	t.Parallel()
	coordinator, err := NewCoordinatorAtMembership[string](Membership{
		GroupID: "orders/v1",
		Epoch:   7,
		Members: []string{"api", "worker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	membership := coordinator.Membership()
	if membership.Epoch != 7 || membership.GroupID != "orders/v1" {
		t.Fatalf("initial membership = %#v", membership)
	}
	tag := crdt.Tag{ReplicaID: "api", WallTime: 1}
	if err := coordinator.Acknowledge("orders/v1", "api", 7, []crdt.Tag{tag}); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.InstallMembership(Membership{GroupID: "orders/v1", Epoch: 9, Members: []string{"api", "mobile"}}); err != nil {
		t.Fatal(err)
	}
	membership = coordinator.Membership()
	if membership.Epoch != 9 || len(membership.Members) != 2 || membership.Members[1] != "mobile" {
		t.Fatalf("installed membership = %#v", membership)
	}
	if stats := coordinator.AcknowledgementStats(); stats.Entries != 0 || stats.Tags != 0 {
		t.Fatalf("receipt state survived new epoch: %#v", stats)
	}
	if err := coordinator.Acknowledge("orders/v1", "api", 7, []crdt.Tag{tag}); !errors.Is(err, ErrStaleMembership) {
		t.Fatalf("old epoch acknowledgement = %v, want %v", err, ErrStaleMembership)
	}
}

func TestCoordinatorRejectsMembershipRollbackAndConflict(t *testing.T) {
	t.Parallel()
	coordinator, err := NewCoordinatorAtMembership[string](Membership{GroupID: "orders/v1", Epoch: 3, Members: []string{"api"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.InstallMembership(Membership{GroupID: "orders/v1", Epoch: 3, Members: []string{"api"}}); err != nil {
		t.Fatalf("idempotent install = %v", err)
	}
	if err := coordinator.InstallMembership(Membership{GroupID: "orders/v1", Epoch: 3, Members: []string{"worker"}}); !errors.Is(err, ErrMembershipRollback) {
		t.Fatalf("same epoch conflicting members = %v", err)
	}
	if err := coordinator.InstallMembership(Membership{GroupID: "orders/v1", Epoch: 2, Members: []string{"api"}}); !errors.Is(err, ErrMembershipRollback) {
		t.Fatalf("rollback install = %v", err)
	}
	if err := coordinator.InstallMembership(Membership{GroupID: "other/v1", Epoch: 4, Members: []string{"api"}}); !errors.Is(err, ErrMembershipGroupMismatch) {
		t.Fatalf("wrong group install = %v", err)
	}
	if _, err := NewCoordinatorAtMembership[string](Membership{GroupID: "orders/v1", Members: []string{"api"}}); !errors.Is(err, ErrInvalidMembership) {
		t.Fatalf("zero epoch constructor = %v", err)
	}
}
