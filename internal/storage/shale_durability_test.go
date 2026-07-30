//go:build slatedb

package storage

// Pins the ShaleConfig.RelaxedDurability -> slate.Config.WriteOptions mapping
// (docs/SPEC.md "Relaxed durability: fast-ack at the memtable"): false, the
// zero value, leaves WriteOptions nil, which slate reads as durable; true sets
// AwaitDurable:false. Pure, so no object store is needed.

import "testing"

func TestSlateConfigFromShale_DefaultIsDurableWriteOptionsNil(t *testing.T) {
	sc := slateConfigFromShale(ShaleConfig{
		Bucket: "b",
		DbName: "db",
	})
	if sc.WriteOptions != nil {
		t.Fatalf("default (RelaxedDurability=false) must leave WriteOptions nil (the durable path), got %+v", sc.WriteOptions)
	}
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
