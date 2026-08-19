// shale_coordinator.go - env parsing + config gates for the shale
// coordination-adapter selection (docs/SPEC.md "Coordination is a pluggable
// choice"). Deliberately OUTSIDE the slatedb build tag: the consumer
// (metadata_shale.go's openShaleRepoFromEnv) only compiles with -tags slatedb,
// but the contract is pure env+string logic, so staying untagged lets the
// default test suite pin it without the slatedb toolchain or MinIO.

package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/Zamua/hostthis/internal/storage"
)

// shaleCoordinatorFromEnv reads the coordination-adapter selection:
//
//	HOSTTHIS_SHALE_COORDINATOR   "" | "gossip" | "cas"
//
// ""/"gossip" (case-insensitive) normalize to the storage layer's default
// path, gossip iff HOSTTHIS_SHALE_BIND_ADDR is set; "cas" normalizes to
// storage.CoordinatorCAS. Any other value is a configuration error: the
// daemon refuses to start rather than coordinating over a silently
// substituted adapter.
func shaleCoordinatorFromEnv() (string, error) {
	raw := strings.TrimSpace(os.Getenv("HOSTTHIS_SHALE_COORDINATOR"))
	switch strings.ToLower(raw) {
	case "":
		return "", nil
	case "gossip":
		// The explicit spelling demands the config that makes it true. Without
		// a bind address every pod would boot as its own single-node cluster
		// over the shared bucket, a fence-fighting fleet no per-pod check can
		// otherwise see. The empty default keeps that combination legal only
		// because existing single-node deploys rely on it.
		if strings.TrimSpace(os.Getenv("HOSTTHIS_SHALE_BIND_ADDR")) == "" {
			return "", fmt.Errorf(
				"HOSTTHIS_SHALE_COORDINATOR=gossip requires HOSTTHIS_SHALE_BIND_ADDR: " +
					"without a bind address each pod runs single-node; leave the " +
					"coordinator unset for a deliberate single-node deploy")
		}
		return "", nil
	case storage.CoordinatorCAS:
		return storage.CoordinatorCAS, nil
	default:
		return "", fmt.Errorf(
			"HOSTTHIS_SHALE_COORDINATOR: unrecognized value %q (want \"\", \"gossip\" or \"cas\")", raw)
	}
}

// checkCoordinatorConfig rejects gossip config under the cas coordinator, and
// cas without the homogeneous bootstrap that supplies its store. The default
// mode constrains nothing: it is exactly today's contract.
//
// A fleet where some pods coordinate over gossip and some over the membership
// document is two half-clusters fencing each other's units - an availability
// outage the storage epochs bound but cannot prevent. This refusal makes a
// SINGLE POD's mixed config unrepresentable; it cannot see the fleet. The
// fleet-level case (old pods on gossip, new pods on cas, every config
// individually valid) arises from any ROLLING flip and is protected by
// operator choreography alone: the adapter switch is a full-stop (scale to
// zero, flip, scale up), never a rolling update.
// seeds is the RAW env value: even a value that parses to no peers (",") is
// gossip config being expressed, so it is refused all the same.
func checkCoordinatorConfig(coordMode, bindAddr, seeds string, homogeneous bool) error {
	if coordMode != storage.CoordinatorCAS {
		return nil
	}
	if bindAddr != "" {
		return fmt.Errorf(
			"HOSTTHIS_SHALE_BIND_ADDR must be unset when HOSTTHIS_SHALE_COORDINATOR=cas: " +
				"a memberlist bind address is gossip config, and a fleet mixing gossip and cas " +
				"coordination splits into two half-clusters fencing each other's storage units")
	}
	if strings.TrimSpace(seeds) != "" {
		return fmt.Errorf(
			"HOSTTHIS_SHALE_SEEDS must be unset when HOSTTHIS_SHALE_COORDINATOR=cas: " +
				"seeds are gossip config, and a fleet mixing gossip and cas coordination " +
				"splits into two half-clusters fencing each other's storage units")
	}
	if !homogeneous {
		return fmt.Errorf(
			"HOSTTHIS_SHALE_COORDINATOR=cas requires HOSTTHIS_SHALE_HOMOGENEOUS=true: " +
				"the homogeneous bootstrap supplies the conditional store the membership document lives in")
	}
	return nil
}

// checkHomogeneousForMode gates the homogeneous bootstrap: it needs the
// sharded multi-backend shape (the marker's durable {gen, count} records the
// unit count) and a multi-node topology, which is a bind address under the
// default coordinator or the cas coordinator, which has no bind address at
// all. Errors name the env vars the operator set, not internal config fields.
func checkHomogeneousForMode(homogeneous bool, unitCount int, bindAddr, coordMode string) error {
	if !homogeneous {
		return nil
	}
	if unitCount <= 0 {
		return fmt.Errorf("HOSTTHIS_SHALE_HOMOGENEOUS=true requires multi-backend mode (HOSTTHIS_SHALE_UNIT_COUNT > 0)")
	}
	if bindAddr == "" && coordMode != storage.CoordinatorCAS {
		return fmt.Errorf("HOSTTHIS_SHALE_HOMOGENEOUS=true requires multi-node mode (HOSTTHIS_SHALE_BIND_ADDR set, or HOSTTHIS_SHALE_COORDINATOR=cas)")
	}
	return nil
}
