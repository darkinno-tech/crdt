package persistence

import "github.com/im10furry/crdt/set"

func setForBenchmark() (*set.ORSet[string], error) {
	value, err := set.NewORSet("maintenance", testStringCodec{})
	if err != nil {
		return nil, err
	}
	for _, item := range []string{"inspect-filter", "replace-filter", "record-service"} {
		if _, err := value.Add(item); err != nil {
			return nil, err
		}
	}
	return value, nil
}
