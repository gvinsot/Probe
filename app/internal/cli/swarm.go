package cli

import (
	"flag"
	"fmt"
	"strings"

	"github.com/gvinsot/Probe/app/internal/config"
	"github.com/gvinsot/Probe/app/internal/reviewer"
)

// swarmFlags are the reviewer swarm flags of review.
type swarmFlags struct {
	enabled *bool
	agents  *string
}

func addSwarmFlags(f *flag.FlagSet) swarmFlags {
	return swarmFlags{
		enabled: f.Bool("swarm", false, "review: investigate with a swarm of specialized reviewer agents in parallel ("+strings.Join(reviewer.SpecializationNames(), ", ")+") instead of one reviewer (default: as policy reviewer.swarm says); --swarm=false runs one reviewer"),
		agents:  f.String("swarm-agents", "", "review: comma-separated swarm agents to run, in order (implies --swarm)"),
	}
}

// resolveSwarm combines the trusted policy and the flags. It runs before any
// repository code or provider call; every error exits 3.
func resolveSwarm(mode string, explicit map[string]bool, flags swarmFlags, policy *config.Swarm, reviewerOn bool) (*reviewer.Swarm, error) {
	if mode == "lint" && (explicit["swarm"] || explicit["swarm-agents"]) {
		return nil, fmt.Errorf("lint never calls a provider; --swarm and --swarm-agents apply to review only")
	}
	var swarm *reviewer.Swarm
	if policy != nil {
		swarm = &reviewer.Swarm{Agents: append([]string(nil), policy.Agents...), MaxParallel: policy.MaxParallel, PartitionFiles: reviewer.DefaultPartitionFiles}
		if policy.PartitionFiles != nil {
			swarm.PartitionFiles = *policy.PartitionFiles
		}
	}
	if explicit["swarm"] {
		switch {
		case !*flags.enabled:
			if explicit["swarm-agents"] {
				return nil, fmt.Errorf("--swarm-agents cannot be combined with --swarm=false")
			}
			swarm = nil
		case swarm == nil:
			swarm = &reviewer.Swarm{PartitionFiles: reviewer.DefaultPartitionFiles}
		}
	}
	if *flags.agents != "" {
		if swarm == nil {
			swarm = &reviewer.Swarm{PartitionFiles: reviewer.DefaultPartitionFiles}
		}
		swarm.Agents = nil
		for _, name := range strings.Split(*flags.agents, ",") {
			if name = strings.TrimSpace(name); name != "" {
				swarm.Agents = append(swarm.Agents, name)
			}
		}
		if len(swarm.Agents) == 0 {
			return nil, fmt.Errorf("--swarm-agents names no agent")
		}
	}
	if swarm != nil && explicit["reviewer"] && !reviewerOn && (explicit["swarm"] || explicit["swarm-agents"]) {
		return nil, fmt.Errorf("--swarm runs reviewer agents; it cannot be combined with --reviewer=false")
	}
	if err := reviewer.ValidateSwarm(swarm); err != nil {
		return nil, err
	}
	return swarm, nil
}
