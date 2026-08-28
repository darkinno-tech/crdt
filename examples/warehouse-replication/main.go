// Command warehouse-replication demonstrates framed G-Set and MV-Register
// replication between warehouse sites and an operations dashboard.
package main

import (
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/im10furry/crdt/register"
	"github.com/im10furry/crdt/set"
)

type stringCodec struct{}

func (stringCodec) ID() string                            { return "example.com/warehouse-item/v1" }
func (stringCodec) Marshal(value string) ([]byte, error)  { return []byte(value), nil }
func (stringCodec) Unmarshal(data []byte) (string, error) { return string(data), nil }

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(writer io.Writer) error {
	items, err := replicateInventory()
	if err != nil {
		return fmt.Errorf("replicate inventory: %w", err)
	}
	statuses, recoveredStatus, err := replicateStatusAndRecover()
	if err != nil {
		return fmt.Errorf("replicate status: %w", err)
	}
	if _, err := fmt.Fprintf(writer, "inventory=%v\nconcurrent-statuses=%v\nrecovered-status=%s\n", items, statuses, recoveredStatus); err != nil {
		return fmt.Errorf("write replication result: %w", err)
	}
	return nil
}

func replicateInventory() ([]string, error) {
	codec := stringCodec{}
	north, err := set.NewGSet("warehouse-north", codec)
	if err != nil {
		return nil, err
	}
	south, err := set.NewGSet("warehouse-south", codec)
	if err != nil {
		return nil, err
	}
	dashboard, err := set.NewGSet("operations", codec)
	if err != nil {
		return nil, err
	}

	northDelta, err := north.Add("filter-17")
	if err != nil {
		return nil, err
	}
	southDelta, err := south.Add("pump-42")
	if err != nil {
		return nil, err
	}
	for _, delta := range []set.GSetDelta[string]{northDelta, southDelta, northDelta} {
		if err := deliverGSet(dashboard, delta, codec); err != nil {
			return nil, err
		}
	}

	items := dashboard.Elements()
	sort.Strings(items)
	return items, nil
}

func replicateStatusAndRecover() ([]string, string, error) {
	north, err := register.NewMVRegister("warehouse-north")
	if err != nil {
		return nil, "", err
	}
	south, err := register.NewMVRegister("warehouse-south")
	if err != nil {
		return nil, "", err
	}
	dashboard, err := register.NewMVRegister("operations")
	if err != nil {
		return nil, "", err
	}

	northDelta, err := north.Set([]byte("maintenance"))
	if err != nil {
		return nil, "", err
	}
	southDelta, err := south.Set([]byte("inspection-required"))
	if err != nil {
		return nil, "", err
	}
	for _, delta := range []register.MVRegisterDelta{northDelta, southDelta} {
		if err := deliverMVRegister(dashboard, delta); err != nil {
			return nil, "", err
		}
	}

	statuses := make([]string, 0, len(dashboard.Values()))
	for _, value := range dashboard.Values() {
		statuses = append(statuses, string(value.Value))
	}
	sort.Strings(statuses)

	saved, err := dashboard.Snapshot()
	if err != nil {
		return nil, "", err
	}
	recovered, err := register.NewMVRegisterFromSnapshot("operations", saved)
	if err != nil {
		return nil, "", err
	}
	if _, err := recovered.Set([]byte("assigned")); err != nil {
		return nil, "", err
	}
	status, ok := recovered.Value()
	if !ok {
		return nil, "", fmt.Errorf("expected one recovered status")
	}
	return statuses, string(status), nil
}

func deliverGSet(target *set.GSet[string], delta set.GSetDelta[string], codec stringCodec) error {
	encoded, err := delta.MarshalBinary(codec)
	if err != nil {
		return err
	}
	received, err := set.UnmarshalGSetDelta(encoded, codec)
	if err != nil {
		return err
	}
	return target.ApplyDelta(received)
}

func deliverMVRegister(target *register.MVRegister, delta register.MVRegisterDelta) error {
	encoded, err := delta.MarshalBinary()
	if err != nil {
		return err
	}
	received, err := register.UnmarshalMVRegisterDelta(encoded)
	if err != nil {
		return err
	}
	return target.ApplyDelta(received)
}
