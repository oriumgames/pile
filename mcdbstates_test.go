package pile

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/sandertv/gophertunnel/minecraft/nbt"
)

// mcdbStorage builds one stored paletted storage the way Bedrock writes it:
// a header byte of (bits << 1) | 1, the packed index words, and the palette.
//
// The palette count is written only when bits is non-zero. A uniform storage
// implies a count of one, which is the case this file exists to pin.
func mcdbStorage(t testing.TB, bits int, names ...string) []byte {
	t.Helper()
	var b bytes.Buffer
	b.WriteByte(byte(bits<<1) | 1)
	if bits > 0 {
		per := 32 / bits
		words := (4096 + per - 1) / per
		b.Write(make([]byte, words*4))
		_ = binary.Write(&b, binary.LittleEndian, uint32(len(names)))
	}
	enc := nbt.NewEncoderWithEncoding(&b, nbt.LittleEndian)
	for _, n := range names {
		if err := enc.Encode(mcdbBlockEntry{Name: n, State: map[string]any{}, Version: 18040335}); err != nil {
			t.Fatal(err)
		}
	}
	return b.Bytes()
}

// mcdbSubChunk wraps storages in a version-9 sub-chunk record.
func mcdbSubChunk(storages ...[]byte) []byte {
	out := []byte{9, byte(len(storages)), 0}
	for _, s := range storages {
		out = append(out, s...)
	}
	return out
}

// TestSubChunkPaletteReadsUniformStorages.
//
// A storage of zero bits per index is Bedrock's uniform section: no index words
// at all, and no palette count, because one entry is implied by the width. The
// scanner treated zero bits as malformed and, when that was fixed, still read
// four bytes for the absent count -- which ate the first tag of the NBT that
// followed.
//
// Between them the two bugs made the scan miss 3 258 of a real world's 21 512
// sub-chunks and report 207 block states where there were 1 241. The states it
// lost were the ones that appear only in uniform storages, which for a map
// built from large single-block volumes is most of what a behaviour pack
// contributes: pile convert --permissive then registered the states the scan
// had found and failed anyway, on a block the report had never mentioned.
func TestSubChunkPaletteReadsUniformStorages(t *testing.T) {
	for _, c := range []struct {
		name string
		sub  []byte
		want []string
	}{
		{
			"a uniform storage carries one entry and no count",
			mcdbSubChunk(mcdbStorage(t, 0, "cubecraft:portal_side")),
			[]string{"cubecraft:portal_side"},
		},
		{
			"a uniform storage beside a wide one",
			mcdbSubChunk(
				mcdbStorage(t, 0, "minecraft:stone"),
				mcdbStorage(t, 4, "minecraft:air", "minecraft:water"),
			),
			[]string{"minecraft:stone", "minecraft:air", "minecraft:water"},
		},
		{
			// The wide storage comes first, so a parser that mis-sizes the
			// uniform one after it corrupts nothing until the very end -- which
			// is how this survived: the failure surfaced far from its cause.
			"a wide storage before a uniform one",
			mcdbSubChunk(
				mcdbStorage(t, 2, "minecraft:dirt", "minecraft:grass"),
				mcdbStorage(t, 0, "cubecraft:portal_corner"),
			),
			[]string{"minecraft:dirt", "minecraft:grass", "cubecraft:portal_corner"},
		},
		{
			"a padded width (three bits) sizes its extra word",
			mcdbSubChunk(mcdbStorage(t, 3, "a", "b", "c", "d", "e")),
			[]string{"a", "b", "c", "d", "e"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := mcdbSubChunkPalette(c.sub)
			if err != nil {
				t.Fatalf("a well-formed sub-chunk was rejected: %v", err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("read %d palette entries, want %d: %+v", len(got), len(c.want), got)
			}
			for i, w := range c.want {
				if got[i].Name != w {
					t.Errorf("entry %d is %q, want %q", i, got[i].Name, w)
				}
			}
		})
	}
}

// TestSubChunkPaletteSkipsAbsentStorages: 0x7F marks a storage that is not
// there, and dragonfly's decoder consumes neither indices nor a palette for it.
func TestSubChunkPaletteSkipsAbsentStorages(t *testing.T) {
	sub := mcdbSubChunk([]byte{0x7F << 1}, mcdbStorage(t, 0, "minecraft:stone"))
	got, err := mcdbSubChunkPalette(sub)
	if err != nil {
		t.Fatalf("an absent storage was rejected: %v", err)
	}
	if len(got) != 1 || got[0].Name != "minecraft:stone" {
		t.Fatalf("read %+v, want just minecraft:stone", got)
	}
}

// TestSubChunkPaletteRefusesWhatItCannotWalk: the scan feeds block-state
// registration, so a sub-chunk it cannot read has to be an error. Carrying on
// is what turned a parser bug into a conversion that failed thousands of chunks
// later naming a block instead.
func TestSubChunkPaletteRefusesWhatItCannotWalk(t *testing.T) {
	for _, c := range []struct {
		name string
		sub  []byte
	}{
		{"truncated", []byte{9}},
		{"a width no storage uses", mcdbSubChunk([]byte{33 << 1})},
		{"indices that are not there", mcdbSubChunk([]byte{4<<1 | 1, 0, 0})},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := mcdbSubChunkPalette(c.sub); err == nil {
				t.Fatal("a malformed sub-chunk was accepted")
			}
		})
	}
}
