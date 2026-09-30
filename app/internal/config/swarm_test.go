package config

import (
	"encoding/json"
	"testing"
)

func TestReviewerSwarmPolicy(t *testing.T) {
	c := Default("go")
	if c.Reviewer.Swarm != nil {
		t.Fatal("the default policy must not enable a swarm: older binaries reject the key")
	}
	data := []byte(`{"agents":["security","tests"],"max_parallel":2,"partition_files":0}`)
	var s Swarm
	if err := json.Unmarshal(data, &s); err != nil || s.PartitionFiles == nil || *s.PartitionFiles != 0 {
		t.Fatalf("partition_files 0 must stay explicit: %+v %v", s, err)
	}
	c.Reviewer.Swarm = &s
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Swarm{{MaxParallel: 9}, {MaxParallel: -1}, {Agents: make([]string, 13)}, {PartitionFiles: intPtr(1001)}} {
		b := bad
		c.Reviewer.Swarm = &b
		if c.Validate() == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func intPtr(v int) *int { return &v }
