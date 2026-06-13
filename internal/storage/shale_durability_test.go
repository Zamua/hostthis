//go:build slatedb

package storage

// Unit test for the relaxed-durability wiring: ShaleConfig.AwaitDurable must
// map to the slate backend's per-write WriteOptions. Pure (no object store),
// so it pins the env-knob -> WriteOptions contract without opening a cluster.
//
// The contract (docs/SPEC.md "Relaxed durability: fast-ack at the memtable"):
//   - The zero value (RelaxedDurability=false, the safe default) leaves
//     slate.Config.WriteOptions nil, so slate takes the byte-exact durable
//     path it took before the knob existed.
//   - RelaxedDurability=true sets WriteOptions to fast-ack
//     (AwaitDurable:false), enabling relaxed durability.

import "testing"

func TestSlateConfigFromShale_DefaultIsDurableWriteOptionsNil(t *testing.T) {
	// Zero value of RelaxedDurability (unset) must stay durable - this is the
	// fail-safe that keeps tests + the migration tool on the durable path.
	sc := slateConfigFromShale(ShaleConfig{
		Bucket: "b",
		DbName: "db",
	})
	if sc.WriteOptions != nil {
		t.Fatalf("default (RelaxedDurability=false) must leave WriteOptions nil (the durable path), got %+v", sc.WriteOptions)
	}
	// The connection fields must still copy through.
	if sc.Bucket != "b" || sc.DbName != "db" {
		t.Fatalf("connection fields not copied through: %+v", sc)
	}
}

func TestSlateConfigFromShale_RelaxedSetsFastAck(t *testing.T) {
	sc := slateConfigFromShale(ShaleConfig{
		Bucket:            "b",
		DbName:            "db",
		RelaxedDurability: true,
	})
	if sc.WriteOptions == nil {
		t.Fatal("RelaxedDurability=true must set WriteOptions for fast-ack, got nil")
	}
	if sc.WriteOptions.AwaitDurable {
		t.Fatalf("relaxed mode must set WriteOptions.AwaitDurable=false, got true")
	}
}
